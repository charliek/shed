//! Surface B — the shed-server plugin message bus (Rust port of `sdk/hostclient.go`
//! + `sdk/envelope.go`).
//!
//! [`BusClient`] is the host-agent's `HostClient` equivalent: it subscribes to a
//! namespace's SSE stream, receives request [`Envelope`]s, and `respond`s with
//! response envelopes routed back to the originating shed. It reuses shed-core's
//! transport primitives — the SSE framer (`shed_core::sse::SseParser`) and the
//! leaf-cert TLS pin verifier (`shed_core::tls::pinned_client_config`) — over a
//! reqwest+rustls client, so the wire behavior matches the Swift/Go clients byte
//! for byte.
//!
//! Wire behavior mirrored from `hostclient.go` EXACTLY:
//!   * subscribe: `GET {server}/api/plugins/listeners/{ns}/messages` with
//!     `Accept: text/event-stream` + an optional `Authorization: Bearer` header;
//!   * reconnect with exponential backoff 1s→30s, reset after a >60s-held
//!     connection, logged loud-then-quiet;
//!   * 409 → terminal (observably `Rejected`, no hot-loop retry);
//!   * 401 → invalidate the token provider then reconnect (subscribe) / retry once
//!     (respond);
//!   * respond: `POST {server}/api/plugins/listeners/{ns}/respond`,
//!     `Content-Type: application/json`, body = the envelope JSON, expects 204;
//!   * TLS pin: install the pin verifier on an https URL, fail-closed if a pin is
//!     set but the URL is not https.
//!
//! Scope: the daemon subscribes only to `ssh-agent` and implements the full
//! ssh-agent op set — the gated **`sign`** flow (decode → approval gate → backend →
//! response + audit, wire-compatible with the Go `ssh_handler.go`), the ungated
//! **`list`** (backend keys → `SSHListResponse` + a durable non-gated audit) and
//! **`status`** (`{connected, mode, key_count}`) ops, and `ping` → `pong`. An unknown
//! operation gets Go's exact `unknown operation: <op>` `INTERNAL_ERROR` envelope, and
//! a payload that isn't a JSON object gets `{invalid payload, INTERNAL_ERROR}` — so the
//! shed's request never hangs. The `aws-credentials` and `docker-credentials` namespaces
//! are wired alongside (each its own per-namespace gate), and the always-on egress-audit
//! SSE consumer ([`crate::egress`]) runs as a side task per server — so the Go and Rust
//! daemons now touch the same endpoint set (the endpoint asymmetry closed with this slice).

use std::collections::BTreeMap;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use base64::Engine as _;
use futures_util::StreamExt;
use serde::{Deserialize, Serialize};
use thiserror::Error;
use tokio::sync::{mpsc, watch};
use tokio::task::JoinHandle;

use shed_core::sse::SseParser;
use shed_core::tls::pinned_client_config;

use crate::approval::ApprovalGate;
use crate::audit::{AuditEntry, AuditSink};
use crate::aws_backend::{aws_expiry_detail, aws_literal_z, AwsBackend};
use crate::config::{NS_AWS_CREDENTIALS, NS_DOCKER_CREDENTIALS, NS_SSH_AGENT};
use crate::discovery::ServerTarget;
use crate::docker_backend::{
    DockerBackend, DOCKER_CODE_INTERNAL, DOCKER_CODE_NOT_ALLOWED, DOCKER_CODE_NOT_FOUND,
};
use crate::ssh_backend::SshBackend;

/// SSH protocol error codes (`internal/ext/protocol/ssh.go`).
const SSH_CODE_KEY_NOT_FOUND: &str = "KEY_NOT_FOUND";
const SSH_CODE_SIGN_FAILED: &str = "SIGN_FAILED";
const SSH_CODE_INTERNAL: &str = "INTERNAL_ERROR";

/// AWS protocol error codes (`internal/ext/protocol/aws.go`). `ASSUME_ROLE_FAILED` is
/// scoped to `get_credentials` failures only (gate-deny / backend-error); the shared
/// unknown-op / invalid-payload dispatch uses `INTERNAL_ERROR` (`AWSCodeInternal`).
/// Go also defines `ROLE_NOT_FOUND`, but the handler NEVER emits it, so it is
/// intentionally omitted here.
const AWS_CODE_ASSUME_ROLE_FAILED: &str = "ASSUME_ROLE_FAILED";
const AWS_CODE_INTERNAL: &str = "INTERNAL_ERROR";
/// The gated AWS op — the only one whose audit carries the approval outcome.
const AWS_OP_GET_CREDENTIALS: &str = "get_credentials";

/// Docker operations (`internal/ext/protocol/docker.go`) used for the audit `op` field
/// / gate op. The remaining ops (`ping`/`status`) match as literals in `docker_dispatch`.
const DOCKER_OP_GET: &str = "get";
const DOCKER_OP_LIST: &str = "list";
/// The audit-only code for a request the approval gate rejected — distinct from
/// `REGISTRY_NOT_ALLOWED` (an allowlist deny). The guest receives
/// `REGISTRY_NOT_ALLOWED` for BOTH (preserving its behavior); this code only
/// disambiguates the cause on host/admin surfaces (mirror Go's `auditCodeApprovalDenied`,
/// `docker_handler.go:32`).
const DOCKER_AUDIT_CODE_APPROVAL_DENIED: &str = "APPROVAL_DENIED";
/// The docker `get` audit result for an allowed registry that had no credential
/// (`CREDENTIALS_NOT_FOUND`): NOT a failure — the guest pulls anonymously — so it is
/// recorded distinctly from a real error (mirror Go's `auditResultAnonymous`).
const DOCKER_AUDIT_RESULT_ANONYMOUS: &str = "anonymous";

// ---------------------------------------------------------------------------
// Constants (mirrors hostclient.go)
// ---------------------------------------------------------------------------

/// Reconnect backoff floor (`hostclient.go:initialBackoff`).
const INITIAL_BACKOFF: Duration = Duration::from_secs(1);
/// Reconnect backoff ceiling (`hostclient.go:maxReconnectBackoff`).
const MAX_BACKOFF: Duration = Duration::from_secs(30);
/// A connection held longer than this resets the backoff to the floor, so a
/// flapping server doesn't keep resetting it (`hostclient.go:subscribeLoop`).
const HELD_RESET: Duration = Duration::from_secs(60);
/// Envelope channel buffer (`hostclient.go:Subscribe` uses `make(chan, 32)`).
const ENVELOPE_CHANNEL_CAP: usize = 32;
/// Max bytes buffered for a single SSE event before the bus tears the connection
/// down and reconnects (surfaced as a transport read error). Mirrors Go's
/// `bufio.Scanner` 1 MiB cap (`hostclient.go:streamMessages`) so a hostile /
/// never-terminating `data:` event can't grow memory unbounded.
const MAX_SSE_EVENT_BYTES: usize = 1 << 20;
/// Max bytes read from a non-2xx response body before giving up. Mirrors Go's
/// `io.LimitReader(resp.Body, 1024)` — a huge error body can't blow up memory or
/// stall teardown.
const MAX_ERROR_BODY_BYTES: usize = 1024;

/// `MessageType` values (`envelope.go`). The namespace HTTP API routes on these.
/// `REQUEST`/`EVENT` are part of the wire vocabulary but not yet emitted by this
/// slice (the daemon only builds `response` envelopes), so they're allowed dead.
#[allow(dead_code)]
pub const MSG_TYPE_REQUEST: &str = "request";
pub const MSG_TYPE_RESPONSE: &str = "response";
#[allow(dead_code)]
pub const MSG_TYPE_EVENT: &str = "event";

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

/// A bus transport error (mirrors the error surfaces of `hostclient.go`).
#[derive(Debug, Error)]
pub enum BusError {
    #[error("transport error: {0}")]
    Transport(String),
    #[error("unexpected status {0}: {1}")]
    BadStatus(u16, String),
    #[error("{0}")]
    Config(String),
}

// ---------------------------------------------------------------------------
// Envelope (serde mirror of sdk/envelope.go)
// ---------------------------------------------------------------------------

/// Identifies the shed instance that originated or is targeted by a message
/// (`envelope.go:ShedInfo`). Copied verbatim from a request onto its response so
/// shed-server can route the reply back to the originating shed.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ShedInfo {
    pub name: String,
    pub backend: String,
    pub server: String,
}

/// The universal plugin message format (`envelope.go:Envelope`). Field order,
/// names, and `omitempty` semantics match the Go struct so the marshaled JSON is
/// wire-identical:
///   * `in_reply_to` is omitted when empty (Go `omitempty`);
///   * `final` is always present (Go has no `omitempty`);
///   * `shed` is omitted when absent;
///   * `payload` is raw JSON — `null` when absent, matching a nil `json.RawMessage`.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Envelope {
    pub id: String,
    pub namespace: String,
    #[serde(rename = "type")]
    pub msg_type: String,
    #[serde(
        rename = "in_reply_to",
        default,
        skip_serializing_if = "String::is_empty"
    )]
    pub in_reply_to: String,
    #[serde(rename = "final")]
    pub is_final: bool,
    pub timestamp: String,
    /// `None` when the wire frame OMITS `payload` entirely — Go's nil
    /// `json.RawMessage`, which the handlers' `json.Unmarshal` REJECTS (→
    /// `invalid payload`), while an explicit `payload: null` parses to a zero
    /// `operation` (→ `unknown operation: `). The `Option` keeps that
    /// distinction (cursor review finding); the custom deserializer is needed
    /// because serde's stock `Option` handling would fold an explicit `null`
    /// into `None` too. A `None` re-serializes as `null`, matching Go's
    /// nil-RawMessage marshaling.
    #[serde(default, deserialize_with = "de_present_payload")]
    pub payload: Option<serde_json::Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub shed: Option<ShedInfo>,
}

/// Deserialize a PRESENT `payload` field — any JSON value, **including an
/// explicit `null`** — to `Some(value)`. Only an omitted field takes the
/// `#[serde(default)]` `None` (Go's nil `json.RawMessage`).
fn de_present_payload<'de, D>(d: D) -> Result<Option<serde_json::Value>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    serde::Deserialize::deserialize(d).map(Some)
}

impl Envelope {
    /// A response envelope linked to an original request (`envelope.go:NewResponse`):
    /// `type="response"`, `final=true`, a fresh id, and the current UTC timestamp.
    /// (Go mints a UUIDv7; a UUIDv4 here is wire-equivalent — the id is opaque to
    /// routing, which correlates on `in_reply_to`.)
    pub fn new_response(
        in_reply_to: &str,
        namespace: &str,
        payload: serde_json::Value,
    ) -> Envelope {
        Envelope {
            id: new_id(),
            namespace: namespace.to_string(),
            msg_type: MSG_TYPE_RESPONSE.to_string(),
            in_reply_to: in_reply_to.to_string(),
            is_final: true,
            timestamp: crate::status::now_rfc3339(),
            payload: Some(payload),
            shed: None,
        }
    }

    /// The `operation` discriminator of the payload (`{"operation": "..."}`) as a
    /// string, if present. Production dispatch now goes through [`parse_operation`]
    /// (which also distinguishes Go's `invalid payload` case); this stays as a small
    /// accessor the envelope-shape tests read, so it is dead in non-test builds.
    #[cfg_attr(not(test), allow(dead_code))]
    pub fn operation(&self) -> Option<&str> {
        self.payload
            .as_ref()
            .and_then(|p| p.get("operation"))
            .and_then(|v| v.as_str())
    }
}

fn new_id() -> String {
    uuid::Uuid::new_v4().to_string()
}

// ---------------------------------------------------------------------------
// Token provider (mirror of sdk.TokenProvider)
// ---------------------------------------------------------------------------

/// Supplies the bus bearer token and refreshes it on demand (`hostclient.go`'s
/// `TokenProvider`). Open-mode servers send no token (no provider); a secure
/// server's provider mints a credentials token. Implementations must be safe for
/// concurrent use — subscribe and respond call `token` from different tasks.
#[async_trait::async_trait]
pub trait TokenProvider: Send + Sync {
    /// The current bearer token, re-minting if expired. `Err` means no token is
    /// available; the client then sends unauthenticated (mirrors Go's setAuth).
    async fn token(&self) -> Result<String, BusError>;
    /// Mark the current token stale so the next `token()` re-mints. Called after a
    /// 401.
    fn invalidate(&self);
}

/// A fixed-token provider (the open-mode / static-token analog of Go's
/// `WithToken`). `invalidate` is a no-op. Constructing with an empty token yields
/// an unauthenticated client.
pub struct StaticTokenProvider {
    token: String,
}

impl StaticTokenProvider {
    pub fn new(token: impl Into<String>) -> Self {
        Self {
            token: token.into(),
        }
    }
}

#[async_trait::async_trait]
impl TokenProvider for StaticTokenProvider {
    async fn token(&self) -> Result<String, BusError> {
        Ok(self.token.clone())
    }
    fn invalidate(&self) {}
}

// ---------------------------------------------------------------------------
// Logging seam
// ---------------------------------------------------------------------------

/// The bus's operational log sink. Kept behind a trait so the daemon can route bus
/// lines to its log file / stderr while tests use a silent or collecting sink. The
/// operational log is not a differential target, so the format is only loosely
/// pinned. `debug` is the "quiet" tier — the file sink drops it, mirroring Go's
/// WARN-level dedup where the per-retry DEBUG lines vanish.
pub trait BusLog: Send + Sync {
    fn info(&self, msg: &str);
    fn warn(&self, msg: &str);
    fn debug(&self, msg: &str);
    fn error(&self, msg: &str);
}

/// The daemon's bus log sink: appends to `log_file` (or stderr when empty),
/// prefixed like the daemon's own `Log`. Interior-mutable + `Send + Sync` so the
/// async bus tasks can share it. Drops `debug` (the quiet tier).
pub struct FileBusLog {
    writer: Mutex<Box<dyn std::io::Write + Send>>,
}

impl FileBusLog {
    pub fn new(log_file: &str) -> FileBusLog {
        let writer: Box<dyn std::io::Write + Send> = if log_file.is_empty() {
            Box::new(std::io::stderr())
        } else {
            match std::fs::OpenOptions::new()
                .create(true)
                .append(true)
                .open(log_file)
            {
                Ok(f) => Box::new(f),
                Err(_) => Box::new(std::io::stderr()),
            }
        };
        FileBusLog {
            writer: Mutex::new(writer),
        }
    }

    fn write(&self, level: &str, msg: &str) {
        if let Ok(mut w) = self.writer.lock() {
            let _ = writeln!(w, "{level} {msg}");
        }
    }
}

impl BusLog for FileBusLog {
    fn info(&self, msg: &str) {
        self.write("INFO ", msg);
    }
    fn warn(&self, msg: &str) {
        self.write("WARN ", msg);
    }
    fn debug(&self, _msg: &str) {
        // Quiet tier: dropped (mirrors Go's WARN-level reconnect-log dedup).
    }
    fn error(&self, msg: &str) {
        self.write("ERROR", msg);
    }
}

// ---------------------------------------------------------------------------
// Subscription state (mirror of sdk.SubStatus / the Conn* states)
// ---------------------------------------------------------------------------

/// A namespace subscription's connection state (`hostclient.go`'s `Conn*`).
/// `Rejected` is terminal — a 409 (another listener owns the namespace) stops the
/// loop instead of hot-looping a retry.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ConnState {
    Reconnecting,
    Connected,
    Stopped,
    Rejected,
}

/// A snapshot of one subscription's state (`hostclient.go:SubStatus`).
#[derive(Debug, Clone)]
pub struct SubStatus {
    pub namespace: String,
    pub state: ConnState,
    pub last_error: String,
}

fn set_state(status: &Arc<Mutex<SubStatus>>, state: ConnState, cause: Option<&str>) {
    let mut s = status.lock().unwrap();
    s.state = state;
    s.last_error = cause.unwrap_or("").to_string();
}

/// How a single `stream_messages` attempt ended, driving the reconnect decision.
enum StreamEnd {
    /// Shutdown was requested mid-connect / mid-stream — return cleanly.
    Shutdown,
    /// 409: another listener owns the namespace → terminal (no retry).
    Conflict(String),
    /// 401: token rejected → invalidate + reconnect.
    Unauthorized(String),
    /// A connect/read transport error → backoff + retry.
    Transport(String),
    /// A non-{200,401,409} status → backoff + retry. The status code is already
    /// embedded in the message; no separate field.
    BadStatus(String),
    /// The server closed a healthy stream → reconnect.
    ClosedByServer(String),
}

// ---------------------------------------------------------------------------
// BusClient
// ---------------------------------------------------------------------------

/// A message-bus client for one shed-server. Cheaply `Clone` (the reqwest client,
/// token provider, and logger are all `Arc`-backed) so the subscribe task and the
/// responder can share one handle.
#[derive(Clone)]
pub struct BusClient {
    /// Base URL with any trailing slash trimmed (`WithServerURL` semantics).
    server_url: String,
    http: reqwest::Client,
    /// Static open-mode token (Go's `WithToken`); used only when there is no
    /// `token_provider`. Empty → unauthenticated.
    static_token: String,
    /// A refreshing token source (Go's `WithTokenProvider`); takes precedence and
    /// is the only path that 401-retries. `None` in open mode.
    token_provider: Option<Arc<dyn TokenProvider>>,
    log: Arc<dyn BusLog>,
    initial_backoff: Duration,
    max_backoff: Duration,
}

impl BusClient {
    /// Build a bus client for `server_url`. `static_token` is the open-mode static
    /// bearer (sent when non-empty and there's no provider); `token_provider`, when
    /// set, supplies a refreshing token and is the only path that 401-retries.
    /// `pin` (`sha256:<hex>`) installs the leaf-cert pin verifier on an https URL;
    /// a pin on a non-https URL is refused (fail-closed, mirroring
    /// `hostclient.go:applyTLSPin`).
    pub fn new(
        server_url: impl Into<String>,
        static_token: String,
        token_provider: Option<Arc<dyn TokenProvider>>,
        pin: Option<String>,
        log: Arc<dyn BusLog>,
    ) -> Result<Self, BusError> {
        let server_url = server_url.into().trim_end_matches('/').to_string();
        let pin = pin.filter(|p| !p.is_empty());
        if pin.is_some() && !server_url.to_lowercase().starts_with("https://") {
            return Err(BusError::Config(format!(
                "TLS pin configured for a non-https URL {server_url}; refusing to send unpinned plaintext"
            )));
        }
        let http = build_http_client(pin.as_deref())?;
        Ok(Self {
            server_url,
            http,
            static_token,
            token_provider,
            log,
            initial_backoff: INITIAL_BACKOFF,
            max_backoff: MAX_BACKOFF,
        })
    }

    /// Subscribe to `namespace`'s SSE stream. Spawns the reconnecting loop and
    /// returns a [`Subscription`] whose `rx` yields inbound envelopes and whose
    /// `status()` reports the live connection state. The loop runs until `shutdown`
    /// flips true or the subscription is terminally rejected (409).
    pub fn subscribe(&self, namespace: &str, shutdown: watch::Receiver<bool>) -> Subscription {
        let (tx, rx) = mpsc::channel::<Envelope>(ENVELOPE_CHANNEL_CAP);
        let status = Arc::new(Mutex::new(SubStatus {
            namespace: namespace.to_string(),
            state: ConnState::Reconnecting,
            last_error: String::new(),
        }));
        let client = self.clone();
        let ns = namespace.to_string();
        let st = status.clone();
        let handle = tokio::spawn(async move {
            client.subscribe_loop(ns, tx, shutdown, st).await;
        });
        Subscription { rx, status, handle }
    }

    async fn subscribe_loop(
        self,
        namespace: String,
        tx: mpsc::Sender<Envelope>,
        shutdown: watch::Receiver<bool>,
        status: Arc<Mutex<SubStatus>>,
    ) {
        let mut backoff = self.initial_backoff;
        let mut down_logged = false;
        // Terminal state defaults to a clean stop; a 409 overrides it to Rejected so
        // the final snapshot surfaces the rejection (mirrors hostclient.go's deferred
        // `setState(stopState, stopCause)`).
        let mut terminal_state = ConnState::Stopped;
        let mut terminal_cause: Option<String> = None;
        loop {
            if *shutdown.borrow() {
                break;
            }
            let start = Instant::now();
            let connected = AtomicBool::new(false);
            let end = self
                .stream_messages(&namespace, &tx, shutdown.clone(), &status, &connected)
                .await;
            let connected = connected.load(Ordering::SeqCst);

            match end {
                StreamEnd::Shutdown => break,
                StreamEnd::Conflict(reason) => {
                    // Terminal: a second broker must be observably rejected, not
                    // silently retry forever (hostclient.go's errSubscribeConflict).
                    self.log.error(&format!(
                        "SSE subscription rejected; another listener owns namespace {namespace} — not retrying: {reason}"
                    ));
                    terminal_state = ConnState::Rejected;
                    terminal_cause = Some(reason);
                    break;
                }
                StreamEnd::Unauthorized(reason) => {
                    // Token rejected — re-mint so the backoff-reconnect authenticates
                    // fresh (only a provider-backed client has anything to invalidate).
                    if let Some(p) = &self.token_provider {
                        p.invalidate();
                    }
                    set_state(&status, ConnState::Reconnecting, Some(&reason));
                    if connected {
                        down_logged = false;
                    }
                    self.log_down(&namespace, &reason, backoff, &mut down_logged);
                }
                StreamEnd::Transport(reason)
                | StreamEnd::BadStatus(reason)
                | StreamEnd::ClosedByServer(reason) => {
                    set_state(&status, ConnState::Reconnecting, Some(&reason));
                    // Reset backoff only after a connection that held for a while.
                    if connected && start.elapsed() > HELD_RESET {
                        backoff = self.initial_backoff;
                    }
                    if connected {
                        down_logged = false;
                    }
                    self.log_down(&namespace, &reason, backoff, &mut down_logged);
                }
            }

            // Back off, but wake immediately on shutdown.
            tokio::select! {
                _ = wait_shutdown(shutdown.clone()) => break,
                _ = tokio::time::sleep(backoff) => {}
            }
            backoff = (backoff * 2).min(self.max_backoff);
        }
        set_state(&status, terminal_state, terminal_cause.as_deref());
        // Dropping `tx` here closes the receiver, ending the responder loop.
    }

    /// Loud-on-the-down-transition, quiet-while-down logging (mirrors
    /// `hostclient.go`): a persistently-unreachable server logs the WARN once, then
    /// drops to DEBUG (suppressed) — no per-cycle flood.
    fn log_down(&self, namespace: &str, reason: &str, backoff: Duration, down_logged: &mut bool) {
        if !*down_logged {
            self.log.warn(&format!(
                "SSE connection lost, reconnecting namespace={namespace} backoff={backoff:?} error={reason}"
            ));
            *down_logged = true;
        } else {
            self.log.debug(&format!(
                "SSE still down, retrying namespace={namespace} backoff={backoff:?} error={reason}"
            ));
        }
    }

    async fn stream_messages(
        &self,
        namespace: &str,
        tx: &mpsc::Sender<Envelope>,
        shutdown: watch::Receiver<bool>,
        status: &Arc<Mutex<SubStatus>>,
        connected: &AtomicBool,
    ) -> StreamEnd {
        let url = format!(
            "{}/api/plugins/listeners/{}/messages",
            self.server_url, namespace
        );
        let mut req = self
            .http
            .get(&url)
            .header(reqwest::header::ACCEPT, "text/event-stream");
        if let Some(tok) = self.bearer().await {
            req = req.bearer_auth(tok);
        }

        // Race the connect against shutdown so a hung dial can't block teardown.
        let resp = tokio::select! {
            _ = wait_shutdown(shutdown.clone()) => return StreamEnd::Shutdown,
            r = req.send() => match r {
                Ok(r) => r,
                Err(e) => return StreamEnd::Transport(format!("connecting: {e}")),
            }
        };

        let st = resp.status().as_u16();
        if st != 200 {
            // Bounded + shutdown-raced error-body read (Go's io.LimitReader 1024):
            // a hostile/huge non-200 body can't blow up memory or block teardown.
            let body = read_error_body(resp, &shutdown).await;
            return match st {
                409 => StreamEnd::Conflict(format!(
                    "namespace already has an active subscriber: {body}"
                )),
                401 => StreamEnd::Unauthorized(format!("unexpected status 401: {body}")),
                other => StreamEnd::BadStatus(format!("unexpected status {other}: {body}")),
            };
        }

        // Connected: reset the down-log, record the state, and stream envelopes.
        connected.store(true, Ordering::SeqCst);
        set_state(status, ConnState::Connected, None);
        self.log
            .info(&format!("SSE connected namespace={namespace}"));

        let mut stream = resp.bytes_stream();
        // Cap a single SSE event at 1 MiB (Go's bufio.Scanner cap): an oversized /
        // never-terminating event surfaces as a read error → disconnect + reconnect.
        let mut parser = SseParser::new().with_max_event_bytes(MAX_SSE_EVENT_BYTES);
        loop {
            let chunk = tokio::select! {
                _ = wait_shutdown(shutdown.clone()) => return StreamEnd::Shutdown,
                c = stream.next() => c,
            };
            match chunk {
                None => return StreamEnd::ClosedByServer("stream closed by server".into()),
                Some(Err(e)) => return StreamEnd::Transport(format!("reading stream: {e}")),
                Some(Ok(bytes)) => {
                    let events = match parser.try_feed(&bytes) {
                        Ok(events) => events,
                        // Over the 1 MiB cap → treat as a read error and reconnect.
                        Err(e) => return StreamEnd::Transport(format!("reading stream: {e}")),
                    };
                    for ev in events {
                        match serde_json::from_str::<Envelope>(&ev.data) {
                            Ok(env) => {
                                // Race the channel send against shutdown so a slow /
                                // stalled consumer (full channel) can't pin teardown.
                                let sent = tokio::select! {
                                    _ = wait_shutdown(shutdown.clone()) => return StreamEnd::Shutdown,
                                    r = tx.send(env) => r,
                                };
                                if sent.is_err() {
                                    // The consumer dropped the receiver (shutdown).
                                    return StreamEnd::Shutdown;
                                }
                            }
                            Err(e) => self.log.warn(&format!("failed to parse SSE event: {e}")),
                        }
                    }
                }
            }
        }
    }

    /// POST a response envelope back to shed-server for routing to the originating
    /// shed. On a provider-backed 401, invalidate + retry once (at-most-once,
    /// mirroring `hostclient.go:Respond`). Expects 204.
    ///
    /// `shutdown` is threaded through every network await (mirroring Go's `ctx`): a
    /// server that accepted the POST but never replies (or a huge error body) can't
    /// pin the daemon — the send + body-read race the shutdown signal and abort
    /// promptly when it fires.
    pub async fn respond(
        &self,
        namespace: &str,
        env: &Envelope,
        shutdown: &watch::Receiver<bool>,
    ) -> Result<(), BusError> {
        let url = format!(
            "{}/api/plugins/listeners/{}/respond",
            self.server_url, namespace
        );
        let body = serde_json::to_vec(env)
            .map_err(|e| BusError::Transport(format!("marshaling response: {e}")))?;

        let mut resp = self.send_respond(&url, &body, shutdown).await?;
        // A credentials token can expire mid-session: on a provider-backed 401,
        // invalidate + retry once (at-most-once, mirroring hostclient.go:Respond).
        if resp.status().as_u16() == 401 {
            if let Some(p) = &self.token_provider {
                p.invalidate();
                resp = self.send_respond(&url, &body, shutdown).await?;
            }
        }

        let st = resp.status().as_u16();
        if st != 204 {
            // Bounded + shutdown-raced (Go's io.LimitReader 1024).
            let body = read_error_body(resp, shutdown).await;
            return Err(BusError::BadStatus(st, body));
        }
        Ok(())
    }

    async fn send_respond(
        &self,
        url: &str,
        body: &[u8],
        shutdown: &watch::Receiver<bool>,
    ) -> Result<reqwest::Response, BusError> {
        let mut req = self
            .http
            .post(url)
            .header(reqwest::header::CONTENT_TYPE, "application/json")
            .body(body.to_vec());
        if let Some(tok) = self.bearer().await {
            req = req.bearer_auth(tok);
        }
        // Race the POST against shutdown: a server that accepts the connection and
        // never replies must not block the daemon's teardown.
        tokio::select! {
            _ = wait_shutdown(shutdown.clone()) => {
                Err(BusError::Transport("sending response: shutting down".into()))
            }
            r = req.send() => {
                r.map_err(|e| BusError::Transport(format!("sending response: {e}")))
            }
        }
    }

    /// The bearer token to send, or `None`. A provider takes precedence (and on a
    /// mint error the client sends unauthenticated, warning once — Go's setAuth);
    /// otherwise the static token is sent when non-empty; else no header.
    async fn bearer(&self) -> Option<String> {
        if let Some(p) = &self.token_provider {
            match p.token().await {
                Ok(t) => (!t.is_empty()).then_some(t),
                Err(e) => {
                    self.log.warn(&format!(
                        "token provider returned no token; sending unauthenticated: {e}"
                    ));
                    None
                }
            }
        } else if !self.static_token.is_empty() {
            Some(self.static_token.clone())
        } else {
            None
        }
    }

    /// Test hook: shrink the reconnect backoff so reconnect tests run fast.
    #[cfg(test)]
    fn with_test_backoff(mut self, initial: Duration, max: Duration) -> Self {
        self.initial_backoff = initial;
        self.max_backoff = max;
        self
    }
}

/// A live namespace subscription: `rx` yields inbound envelopes; `status()` reports
/// the connection state. Dropping it aborts the loop.
pub struct Subscription {
    pub rx: mpsc::Receiver<Envelope>,
    status: Arc<Mutex<SubStatus>>,
    handle: JoinHandle<()>,
}

impl Subscription {
    /// A snapshot of the subscription's current connection state — including
    /// `Rejected`, the observable 409-terminal (`hostclient.go:Status`).
    pub fn status(&self) -> SubStatus {
        self.status.lock().unwrap().clone()
    }

    /// A shared handle to the subscription's live status, so a watcher group can
    /// register it with the supervisor and `Supervisor::health()` can read the
    /// per-namespace state (incl. the 409 `Rejected` terminal) for `servers[]` long
    /// after this `Subscription` is owned by its serve task (mirror Go's
    /// `HostClient.Status()`, which the supervisor reads off its stored client).
    pub fn status_handle(&self) -> Arc<Mutex<SubStatus>> {
        self.status.clone()
    }
}

impl Drop for Subscription {
    fn drop(&mut self) {
        self.handle.abort();
    }
}

/// Resolve when `shutdown` is (or becomes) true; returns immediately if already
/// flagged. Idempotent + cancel-safe, so it's reusable across select arms (unlike
/// `changed()`, which only fires on a fresh change).
pub(crate) async fn wait_shutdown(mut shutdown: watch::Receiver<bool>) {
    let _ = shutdown.wait_for(|flagged| *flagged).await;
}

/// Read at most [`MAX_ERROR_BODY_BYTES`] of a (non-2xx) response body, racing each
/// read against shutdown. Mirrors Go's `io.LimitReader(resp.Body, 1024)`: a hostile
/// / huge error body can't grow memory unbounded, and a stalled body read can't pin
/// the daemon's teardown. Returns the (trimmed, lossy-decoded) prefix; on a read
/// error or shutdown it returns whatever was read so far.
async fn read_error_body(mut resp: reqwest::Response, shutdown: &watch::Receiver<bool>) -> String {
    let mut buf: Vec<u8> = Vec::new();
    loop {
        let chunk = tokio::select! {
            _ = wait_shutdown(shutdown.clone()) => break,
            c = resp.chunk() => c,
        };
        match chunk {
            Ok(Some(bytes)) => {
                let take = (MAX_ERROR_BODY_BYTES - buf.len()).min(bytes.len());
                buf.extend_from_slice(&bytes[..take]);
                if buf.len() >= MAX_ERROR_BODY_BYTES {
                    break;
                }
            }
            Ok(None) | Err(_) => break,
        }
    }
    String::from_utf8_lossy(&buf).trim().to_string()
}

/// Build the reqwest client. Fail-closed on a plaintext redirect (mirrors Swift's
/// pinned session + shed-core's `build_http_client`); with a `pin` it installs the
/// leaf-cert verifier. No global timeout — the SSE subscribe holds a long-lived
/// connection, and shutdown/`ctx` cancellation bound it instead.
fn build_http_client(pin: Option<&str>) -> Result<reqwest::Client, BusError> {
    let mut builder =
        reqwest::Client::builder().redirect(reqwest::redirect::Policy::custom(|attempt| {
            if attempt.url().scheme() == "https" {
                attempt.follow()
            } else {
                attempt.stop()
            }
        }));
    if let Some(pin) = pin {
        let cfg = pinned_client_config(pin).map_err(|e| BusError::Config(e.to_string()))?;
        builder = builder.use_preconfigured_tls(cfg);
    }
    builder
        .build()
        .map_err(|e| BusError::Transport(e.to_string()))
}

// ---------------------------------------------------------------------------
// Single-server daemon entry point + ping responder
// ---------------------------------------------------------------------------

/// The bus's injected seams for the gated credential flows — built in `main.rs` and
/// threaded through the subscribe loops: the ssh approval `gate` (selected from
/// `ssh.approval.policy`), the `audit` sink (durable JSONL + desktop fan-out; SHARED
/// across namespaces), the ed25519 sign `backend`, the discovery `server_name` (the
/// audit/approval `server` field — empty in single-server mode), and the optional
/// [`AwsHandlers`] (the second bus namespace). Grouping the seams keeps the entry
/// points below the argument-count lint and gives later slices (docker backend, the
/// credential minter) one place to grow.
///
/// Per group, `spawn_server_group` rebuilds one by manual field construction from the
/// shared [`crate::supervisor::SharedDeps`], stamping only `server_name` per target
/// (every other field is an `Arc`/`Option<Arc>` — a cheap pointer clone).
pub struct BusHandlers {
    pub gate: Arc<dyn ApprovalGate>,
    pub audit: Arc<dyn AuditSink>,
    pub backend: Arc<dyn SshBackend>,
    pub server_name: String,
    /// The aws-credentials handler's seams, present only when `aws.enabled()` and the
    /// backend constructed (Go main.go:166-173 — a nil AWS backend means no aws
    /// handler). `None` ⇒ the aws-credentials namespace is never subscribed.
    pub aws: Option<AwsHandlers>,
    /// The docker-credentials handler's seams. Unlike `aws`, this is `Some` in the
    /// COMMON case even when unconfigured: `new_docker_backend` errors ONLY when an
    /// explicit `config_path` is unstat-able (Go `NewDockerBackend`, main.go:175-180),
    /// so `main.rs` all but always sets it — the docker-credentials namespace is
    /// subscribed for every server (denying everything under an empty allowlist). `None`
    /// only on that explicit-config_path error.
    pub docker: Option<DockerHandlers>,
}

/// The aws-credentials handler's seams (the second bus namespace). Carries its OWN
/// per-namespace approval `gate` (selected from `aws.approval.policy`, NEVER ssh's —
/// panel F6) and the `backend`; the audit sink + `server_name` are shared from
/// [`BusHandlers`]. Present iff `main.rs` built the AWS backend.
#[derive(Clone)]
pub struct AwsHandlers {
    pub backend: Arc<dyn AwsBackend>,
    pub gate: Arc<dyn ApprovalGate>,
}

/// The docker-credentials handler's seams (the third bus namespace). Carries its OWN
/// per-namespace approval `gate` (selected from `docker.approval.policy`, NEVER ssh's or
/// aws's) and the `backend`; the audit sink + `server_name` are shared from
/// [`BusHandlers`]. Present in the common case (the constructor near-always yields a live
/// backend — see [`BusHandlers::docker`]).
#[derive(Clone)]
pub struct DockerHandlers {
    pub backend: Arc<dyn DockerBackend>,
    pub gate: Arc<dyn ApprovalGate>,
}

/// The live handles the supervisor keeps for one watcher group ([`spawn_server_group`]'s
/// return): `cancel` tears the group down individually (a reconcile drop), `done` resolves
/// once every group task has exited (drained after releasing the supervisor lock), and
/// `statuses` are the per-namespace [`SubStatus`] handles `Supervisor::health()` reads for
/// `servers[]`.
pub struct GroupHandles {
    /// Flip to `true` to cancel just this group (the supervisor holds it; the parent
    /// shutdown also flips it via the bridge task). `Arc` so both the supervisor and the
    /// parent-bridge task can send (a `watch::Sender` is not itself `Clone`).
    pub cancel: Arc<watch::Sender<bool>>,
    /// Joins every spawned group task (per-namespace serve loops + refresh + egress).
    pub done: JoinHandle<()>,
    /// Shared per-namespace status handles for `health()` (incl. the 409 `Rejected`
    /// terminal) — read long after the `Subscription`s are owned by their serve tasks.
    pub statuses: Vec<Arc<Mutex<SubStatus>>>,
}

/// Spawn one shed server's watcher group — a MECHANICAL GENERALIZATION of the former
/// `run_single_server_bus` (its body, now driven per-[`ServerTarget`] by the supervisor):
/// subscribe to `ssh-agent` (always), `aws-credentials` (when the AWS backend is
/// configured), and `docker-credentials` (when the docker backend constructed — the COMMON
/// case, even unconfigured), plus the always-on egress-audit SSE side task, answering
/// inbound requests until the group's `cancel` (or the parent `shutdown`) flips. Each
/// namespace runs its own subscribe+serve loop, mirroring the Go daemon's per-namespace
/// watcher goroutines (`main.go` → `startWatcherGroup` → the per-handler `.Run`).
///
/// The four deltas from the old single-server body (all supervisor-driven): (a) `server_name`
/// comes from `target.name` — **empty `""` for the single unnamed target** (the `servers[]`
/// "(default)" render); (b) the provider + TLS pin come from [`crate::supervisor::should_mint`]
/// — a credentials-scope `CredentialSource` bridged onto [`TokenProvider`] + `target.tls_fingerprint`
/// when secure, else `StaticTokenProvider(target.token)` and no pin (the unnamed target has
/// `ssh_host=""` → open/no-pin, behavior-identical to the old hardcoded-open path); (c) the
/// egress side task uses a **control-scope** `CredentialSource` when secure (Go's second
/// per-server source), else the static token; (d) the credentials source's `refresh_loop`
/// is spawned as a group task when secure. Each subscription's `status_handle()` is
/// registered so `health()` can read it.
///
/// Returns synchronously (the subscribes + spawns are non-async) so the supervisor can
/// register `statuses` at group-construction time — the reason this is a spawn-and-return
/// helper rather than an awaited `run_*` fn.
pub fn spawn_server_group(
    parent_shutdown: watch::Receiver<bool>,
    target: &ServerTarget,
    deps: &crate::supervisor::SharedDeps,
) -> GroupHandles {
    let log = deps.log.clone();

    // A per-group cancel channel: the supervisor cancels this group directly, and a bridge
    // task flips it when the parent daemon shutdown flips (Go's `context.WithCancel(parent)`
    // — the child ctx is done on EITHER the parent OR the group's own cancel).
    let (cancel_tx, cancel_rx) = watch::channel(false);
    let cancel = Arc::new(cancel_tx);
    {
        let cancel = cancel.clone();
        tokio::spawn(async move {
            wait_shutdown(parent_shutdown).await;
            let _ = cancel.send(true);
        });
    }

    // Provider + pin from should_mint (Go's `startWatcherGroup`): a SECURE server
    // (https + ssh endpoint + minter) authenticates with a self-minted, auto-refreshing
    // credentials-scope token bridged onto the bus `TokenProvider`, pinned to
    // `tls_fingerprint`; an OPEN server sends its (usually empty) static token with no pin.
    let secure = crate::supervisor::should_mint(deps, target);
    let mut refresh_source: Option<Arc<crate::minter::CredentialSource>> = None;
    let (provider, pin): (Arc<dyn TokenProvider>, Option<String>) = if secure {
        let minter = deps
            .minter
            .clone()
            .expect("should_mint implies a minter is present");
        let src = crate::minter::new_credential_source(
            minter,
            target.clone(),
            crate::minter::SCOPE_CREDENTIALS,
        );
        refresh_source = Some(src.clone());
        // `Arc<CredentialSource>: TokenProvider` (the bridge in `supervisor.rs`) — box it
        // into the trait object the client holds.
        (
            Arc::new(src) as Arc<dyn TokenProvider>,
            Some(target.tls_fingerprint.clone()),
        )
    } else {
        (
            Arc::new(StaticTokenProvider::new(target.token.clone())) as Arc<dyn TokenProvider>,
            None,
        )
    };

    let client = match BusClient::new(
        target.url.clone(),
        String::new(),
        Some(provider),
        pin,
        log.clone(),
    ) {
        Ok(c) => c,
        Err(e) => {
            log.error(&format!(
                "message bus disabled server={} url={}: {e}",
                target.name, target.url
            ));
            // An empty group (no subscriptions) whose `done` resolves immediately, so a
            // reconcile drain doesn't hang on a group that never started.
            let done = tokio::spawn(async {});
            return GroupHandles {
                cancel,
                done,
                statuses: Vec::new(),
            };
        }
    };

    // The subscription set: always ssh-agent; aws-credentials when the AWS backend is
    // configured; docker-credentials when the docker backend constructed (near-always).
    let mut subscribed: Vec<&'static str> = vec![NS_SSH_AGENT];
    if deps.aws.is_some() {
        subscribed.push(NS_AWS_CREDENTIALS);
    }
    if deps.docker.is_some() {
        subscribed.push(NS_DOCKER_CREDENTIALS);
    }
    let deferred: Vec<&'static str> = BUS_NAMESPACES
        .iter()
        .copied()
        .filter(|ns| !subscribed.contains(ns))
        .collect();
    log.info(&format!(
        "message bus subscribing server={} namespaces={subscribed:?}; deferred: {deferred:?}; \
         egress side task: always-on",
        target.name
    ));

    // Rebuild the per-group `BusHandlers` from the shared deps, stamping this target's
    // `server_name` (every other field is an Arc clone).
    let handlers = Arc::new(BusHandlers {
        gate: deps.ssh_gate.clone(),
        audit: deps.audit.clone(),
        backend: deps.ssh_backend.clone(),
        server_name: target.name.clone(),
        aws: deps.aws.clone(),
        docker: deps.docker.clone(),
    });

    // Subscribe synchronously (so `statuses` is ready for health()), then hand each
    // `Subscription` to its serve loop. Each namespace subscribes + serves independently
    // (both racing the group cancel), so a slow op on one can't stall the others.
    let mut tasks: Vec<JoinHandle<()>> = Vec::new();
    let mut statuses: Vec<Arc<Mutex<SubStatus>>> = Vec::new();
    for namespace in subscribed {
        let sub = client.subscribe(namespace, cancel_rx.clone());
        statuses.push(sub.status_handle());
        tasks.push(tokio::spawn(serve_namespace(
            client.clone(),
            sub,
            namespace,
            cancel_rx.clone(),
            handlers.clone(),
            log.clone(),
        )));
    }

    // Proactively re-mint the credentials token (jittered) for a secure server so an idle
    // server's bus token stays fresh and a reconnect never pays the mint latency inline.
    if let Some(src) = refresh_source {
        tasks.push(tokio::spawn(src.refresh_loop(cancel_rx.clone())));
    }

    // The always-on egress-audit SSE consumer (Go's watcher group GETs `/api/egress/stream`
    // for every server). NOT a bus namespace: a read-only side task racing the group cancel.
    // A SECURE server gets its OWN control-scope source (the bus token is credentials-scope;
    // the egress route is control-scoped) — NOT refresh-looped (bus-only). An OPEN server
    // passes `None` and sends the static config token.
    let egress_tokens: Option<Arc<dyn crate::egress::EgressTokenSource>> = if secure {
        let minter = deps
            .minter
            .clone()
            .expect("should_mint implies a minter is present");
        let control = crate::minter::new_credential_source(
            minter,
            target.clone(),
            crate::minter::SCOPE_CONTROL,
        );
        Some(Arc::new(control) as Arc<dyn crate::egress::EgressTokenSource>)
    } else {
        None
    };
    let egress = crate::egress::EgressSubscriber::new(
        target.name.clone(),
        target.url.clone(),
        target.token.clone(),
        &target.tls_fingerprint,
        egress_tokens,
        deps.audit.clone(),
        log.clone(),
    );
    let egress_cancel = cancel_rx.clone();
    tasks.push(tokio::spawn(async move {
        egress.run(egress_cancel).await;
    }));

    log.info(&format!(
        "watching server server={} url={}",
        target.name, target.url
    ));
    let done = tokio::spawn(async move {
        for t in tasks {
            let _ = t.await;
        }
    });
    GroupHandles {
        cancel,
        done,
        statuses,
    }
}

/// Subscribe to one namespace's SSE stream and answer inbound requests until `shutdown`
/// flips (or the subscription is terminally rejected). On shutdown (or a terminal 409)
/// the subscribe loop drops its sender, closing `rx` and ending this loop; the recv is
/// also raced against shutdown, and `shutdown` is threaded into the dispatch so a
/// `respond` to a hung server can't pin the loop past a SIGTERM/SIGINT. Dispatch is
/// namespace-aware (see [`dispatch_bus_message`]).
///
/// The `Subscription` is created by the caller ([`spawn_server_group`]) and passed in, so
/// its `status_handle()` can be registered for `health()` before this loop runs — the only
/// structural change from the former inline `client.subscribe(...)`; the dispatch is
/// otherwise untouched.
async fn serve_namespace(
    client: BusClient,
    mut sub: Subscription,
    namespace: &'static str,
    shutdown: watch::Receiver<bool>,
    handlers: Arc<BusHandlers>,
    log: Arc<dyn BusLog>,
) {
    loop {
        let env = tokio::select! {
            _ = wait_shutdown(shutdown.clone()) => break,
            e = sub.rx.recv() => match e {
                Some(env) => env,
                None => break, // sender dropped: shutdown or terminal 409
            },
        };
        dispatch_bus_message(&client, namespace, &env, &shutdown, &handlers).await;
    }
    let s = sub.status();
    log.info(&format!(
        "message bus stopped namespace={} state={:?} last_error={}",
        s.namespace, s.state, s.last_error
    ));
}

/// Route one inbound request to its namespace handler: `aws-credentials` → the gated
/// AWS flow, `docker-credentials` → the docker flow (each with its OWN per-namespace
/// gate + its own error codes), every other namespace (ssh-agent) → [`handle_bus_message`].
/// The aws/docker branches are reached only when their handlers are `Some` (their loops
/// are started only then); a defensive fall-through to the ssh dispatch keeps a stray
/// frame from hanging the shed's request.
async fn dispatch_bus_message(
    client: &BusClient,
    namespace: &str,
    env: &Envelope,
    shutdown: &watch::Receiver<bool>,
    handlers: &BusHandlers,
) {
    match (namespace, &handlers.aws, &handlers.docker) {
        (NS_AWS_CREDENTIALS, Some(aws), _) => {
            handle_aws_bus_message(
                client,
                namespace,
                env,
                shutdown,
                aws,
                &handlers.audit,
                &handlers.server_name,
            )
            .await
        }
        (NS_DOCKER_CREDENTIALS, _, Some(docker)) => {
            handle_docker_bus_message(
                client,
                namespace,
                env,
                shutdown,
                docker,
                &handlers.audit,
                &handlers.server_name,
            )
            .await
        }
        _ => handle_bus_message(client, namespace, env, shutdown, handlers).await,
    }
}

/// Answer one inbound bus request, mirroring `ssh_handler.go:handleMessage`'s
/// dispatch. `ping` → `{"status":"ok"}`; `sign` → the gated flow (approval gate →
/// backend → response + audit); `list` → the ungated key listing + a durable
/// non-gated audit; `status` → `{connected, mode, key_count}` (no audit). An unknown
/// operation → Go's exact `unknown operation: <op>` (`<op>` empty when the field is
/// absent); a payload that is not a JSON object (or null) → `{invalid payload,
/// INTERNAL_ERROR}` — both fail the shed's request cleanly instead of hanging. The
/// reply plumbing (mint response, echo `shed`, POST, warn-on-failure) is shared; the
/// audit is written AFTER the response, matching `ssh_handler.go` (sendResponse/
/// sendError then Log/LogEntry).
async fn handle_bus_message(
    client: &BusClient,
    namespace: &str,
    env: &Envelope,
    shutdown: &watch::Receiver<bool>,
    handlers: &BusHandlers,
) {
    let shed_name = env.shed.as_ref().map(|s| s.name.as_str()).unwrap_or("");
    let (payload, audit_entry, what) = match parse_operation(&env.payload) {
        // A non-object/non-null payload (or a non-string `operation`) is Go's
        // `json.Unmarshal(payload, &op)` error → {invalid payload, INTERNAL_ERROR}; no audit.
        Err(()) => (
            ssh_error("invalid payload", SSH_CODE_INTERNAL),
            None,
            "error response",
        ),
        Ok(op) => match op.as_str() {
            "ping" => (serde_json::json!({"status": "ok"}), None, "ping response"),
            "sign" => {
                let (payload, entry) = handle_sign(
                    env,
                    &handlers.server_name,
                    shed_name,
                    &handlers.gate,
                    &handlers.backend,
                )
                .await;
                (payload, entry, "sign response")
            }
            "list" => {
                let (payload, entry) =
                    handle_list(&handlers.server_name, shed_name, &handlers.backend);
                (payload, Some(entry), "list response")
            }
            "status" => (handle_status(&handlers.backend), None, "status response"),
            // Go's `default` arm: `unknown operation: <op>` (`<op>` empty when absent).
            other => (
                ssh_error(&format!("unknown operation: {other}"), SSH_CODE_INTERNAL),
                None,
                "error response",
            ),
        },
    };
    // The reply plumbing + audit-after-response tail is shared with the aws (and
    // later docker) namespaces (`respond_and_audit`). A non-gated op or a
    // parse-error path leaves `audit_entry` None (Go emits no audit for those).
    respond_and_audit(
        client,
        namespace,
        env,
        shutdown,
        &handlers.audit,
        payload,
        audit_entry,
        what,
    )
    .await;
}

/// The shared reply-plumbing + audit tail for a bus namespace handler, extracted
/// byte-for-byte from the ssh and aws handlers (the rule-of-three the docker slice
/// justifies). Mint the response envelope, echo `shed` so shed-server routes the
/// reply back to the originating shed (`*_handler.go: resp.Shed = req.Shed`), POST it
/// racing `shutdown` (see [`BusClient::respond`]) and warn on failure with a
/// namespace-specific `what`, then — matching Go's sendResponse/sendError-then-Log
/// order — write the audit entry if one is present. `audit` is the SHARED sink taken
/// by reference; `what` absorbs the ssh-vs-aws warn-message difference.
#[allow(clippy::too_many_arguments)]
async fn respond_and_audit(
    client: &BusClient,
    namespace: &str,
    env: &Envelope,
    shutdown: &watch::Receiver<bool>,
    audit: &Arc<dyn AuditSink>,
    payload: serde_json::Value,
    audit_entry: Option<AuditEntry>,
    what: &str,
) {
    let mut resp = Envelope::new_response(&env.id, namespace, payload);
    resp.shed = env.shed.clone(); // route the reply back (resp.Shed = req.Shed)
    if let Err(e) = client.respond(namespace, &resp, shutdown).await {
        client.log.warn(&format!("failed to send {what}: {e}"));
    }
    // Audit after responding — the durable log + desktop fan-out.
    if let Some(entry) = audit_entry {
        audit.log(entry);
    }
}

/// The sign request payload (`internal/ext/protocol/ssh.go:SSHSignRequest`).
/// `operation` is already dispatched on, so it's not needed here. `#[serde(default)]`
/// on every field so a missing field zero-fills (Go `json.Unmarshal` semantics); a
/// wrong-typed field still fails the decode (Go too).
#[derive(Deserialize)]
struct SignRequestPayload {
    #[serde(default)]
    public_key: String,
    #[serde(default)]
    data: String,
    #[serde(default)]
    flags: u32,
}

// ---------------------------------------------------------------------------
// SSH response payload types (serde mirrors of internal/ext/protocol/ssh.go).
// Built as typed structs (not ad-hoc json!) so the field/tag names are pinned in one
// place and the golden runner can exercise the same types — the drift guard the live
// differential complements (see `golden_ssh_payload_shapes`).
// ---------------------------------------------------------------------------

/// `SSHKeyInfo` — one public key in a `list` response. `blob` is base64 of the
/// SSH-wire marshaled public key.
#[derive(Serialize)]
struct SshKeyInfoResp {
    format: String,
    blob: String,
    comment: String,
}

/// `SSHListResponse` — the `list` op's success payload. An empty backend serializes
/// to `{"keys":[]}` (Go's `make([]SSHKeyInfo, 0)` marshals to `[]`, not `null`).
#[derive(Serialize)]
struct SshListResponse {
    keys: Vec<SshKeyInfoResp>,
}

/// `SSHSignResponse` — the `sign` op's success payload. `rest` is always present and
/// pinned to `""` (Go keeps `ssh.Signature.Rest`; the daemon never has trailing bytes).
#[derive(Serialize)]
struct SshSignResponse {
    format: String,
    blob: String,
    rest: String,
}

/// `SSHStatusResponse` — the `status` op's payload.
#[derive(Serialize)]
struct SshStatusResponse {
    connected: bool,
    mode: String,
    key_count: usize,
}

/// `SSHErrorResponse` — an `{error, code}` error payload.
#[derive(Serialize)]
struct SshErrorResponse {
    error: String,
    code: String,
}

/// Serialize a bus response payload struct (ssh/aws/docker) to a
/// `serde_json::Value`. The response structs are all plain
/// `String`/`bool`/`usize`/map/`Vec<struct>` fields, so `to_value` is infallible;
/// `.expect` fails loud on the impossible rather than silently emitting a
/// divergent shape (the typed structs are the single source of the wire field/tag names).
fn to_payload<T: Serialize>(value: &T) -> serde_json::Value {
    serde_json::to_value(value).expect("serialize bus response payload")
}

/// Build an `{error, code}` SSH error payload (`SSHErrorResponse`). shed-server parses
/// it order-insensitively and the differential compares canonically, so key order is
/// not load-bearing.
fn ssh_error(msg: &str, code: &str) -> serde_json::Value {
    to_payload(&SshErrorResponse {
        error: msg.to_string(),
        code: code.to_string(),
    })
}

/// Extract the request `operation`, mirroring Go's
/// `json.Unmarshal(env.Payload, &struct{Operation string})` (`ssh_handler.go:59-66`):
/// a JSON **object** (absent/`null` `operation` → `""`) or an explicit `null` payload
/// parse cleanly; an OMITTED payload field (Go nil `json.RawMessage` — "unexpected
/// end of JSON input"), a non-object/non-null payload, or an `operation` that isn't
/// a string/null, is an unmarshal error → `{invalid payload, INTERNAL_ERROR}`.
fn parse_operation(payload: &Option<serde_json::Value>) -> Result<String, ()> {
    match payload {
        None => Err(()), // omitted field: Go json.Unmarshal(nil, ...) errors
        Some(serde_json::Value::Null) => Ok(String::new()),
        Some(serde_json::Value::Object(map)) => match map.get("operation") {
            None | Some(serde_json::Value::Null) => Ok(String::new()),
            Some(serde_json::Value::String(s)) => Ok(s.clone()),
            Some(_) => Err(()),
        },
        Some(_) => Err(()),
    }
}

/// The `list` op — a faithful port of `ssh_handler.go:handleList`. NO approval gate.
/// Success → `SSHListResponse{keys:[{format, blob(b64), comment}]}` + a durable audit
/// via Go's **positional** `AuditLogger.Log` form (result:"ok", detail:"N keys",
/// approval:"none", and NO decided_by/scope/ttl — distinct from the gated `sign_audit`).
/// A backend error → `{key listing failed, INTERNAL_ERROR}` + audit result:"error",
/// detail = the backend error string, approval:"none". Unlike `sign`, `list` ALWAYS
/// audits.
fn handle_list(
    server_name: &str,
    shed_name: &str,
    backend: &Arc<dyn SshBackend>,
) -> (serde_json::Value, AuditEntry) {
    match backend.list() {
        Ok(keys) => {
            let resp = SshListResponse {
                keys: keys
                    .iter()
                    .map(|k| SshKeyInfoResp {
                        format: k.format.clone(),
                        blob: base64::engine::general_purpose::STANDARD.encode(&k.blob),
                        comment: k.comment.clone(),
                    })
                    .collect(),
            };
            let payload = to_payload(&resp);
            let entry = list_audit(server_name, shed_name, "ok", &format!("{} keys", keys.len()));
            (payload, entry)
        }
        Err(e) => {
            let entry = list_audit(server_name, shed_name, "error", &e);
            (ssh_error("key listing failed", SSH_CODE_INTERNAL), entry)
        }
    }
}

/// The `status` op — a faithful port of `ssh_handler.go:handleStatus`:
/// `{connected:true, mode, key_count}`. `key_count` = `list().len()`, or **0** on a
/// list error (Go computes the count only when `err == nil`). NO audit.
fn handle_status(backend: &Arc<dyn SshBackend>) -> serde_json::Value {
    let key_count = backend.list().map(|k| k.len()).unwrap_or(0);
    let resp = SshStatusResponse {
        connected: true,
        mode: backend.mode().to_string(),
        key_count,
    };
    to_payload(&resp)
}

/// A non-gated audit entry (Go's positional `AuditLogger.Log`): the fixed ssh-agent
/// ns + `op:"list"` with the given result/detail, `approval:"none"`, and NO
/// decided_by/scope/ttl. Distinct from `sign_audit` (which carries the gate outcome).
fn list_audit(server_name: &str, shed_name: &str, result: &str, detail: &str) -> AuditEntry {
    AuditEntry {
        server: server_name.to_string(),
        shed: shed_name.to_string(),
        ns: NS_SSH_AGENT.to_string(),
        op: "list".to_string(),
        result: result.to_string(),
        detail: detail.to_string(),
        approval: "none".to_string(),
        ..Default::default()
    }
}

/// An [`AuditEntry`] scaffold for a sign op with the fixed ns/op + the shared
/// server/shed/approval/outcome fields (Go copies these into every sign audit).
fn sign_audit(
    server_name: &str,
    shed_name: &str,
    result: &str,
    detail: &str,
    approval: String,
    outcome: &crate::approval::ApprovalOutcome,
) -> AuditEntry {
    AuditEntry {
        server: server_name.to_string(),
        shed: shed_name.to_string(),
        ns: NS_SSH_AGENT.to_string(),
        op: "sign".to_string(),
        result: result.to_string(),
        detail: detail.to_string(),
        approval,
        decided_by: outcome.decided_by.clone(),
        scope: outcome.scope.clone().unwrap_or_default(),
        ttl: outcome.ttl.clone().unwrap_or_default(),
        ..Default::default()
    }
}

/// Standard-base64 decode that first strips `\r`/`\n`, matching Go's
/// `base64.StdEncoding.DecodeString` (which silently skips CR/LF anywhere in the input,
/// e.g. line-wrapped base64). The Rust `base64` engine rejects them, so a wrapped
/// `public_key`/`data` would spuriously fail encoding vs the Go daemon. Only CR/LF are
/// stripped — Go does NOT skip spaces/tabs, so neither do we.
fn decode_b64_lenient(s: &str) -> Result<Vec<u8>, base64::DecodeError> {
    if s.as_bytes().iter().any(|&b| b == b'\n' || b == b'\r') {
        let cleaned: String = s.chars().filter(|&c| c != '\n' && c != '\r').collect();
        base64::engine::general_purpose::STANDARD.decode(cleaned)
    } else {
        base64::engine::general_purpose::STANDARD.decode(s)
    }
}

/// The gated `sign` flow — a faithful port of `ssh_handler.go:handleSign`. Returns
/// the response payload (a `SSHSignResponse` on success, else a `SSHErrorResponse`)
/// plus the audit entry to write (only the gate-deny / backend-error / success paths
/// audit; the parse-error paths do NOT, matching Go).
///
/// ORDER matches Go EXACTLY (catalog §6.1 lists gate-deny first): decode request →
/// **approval gate** → decode+parse public key → decode challenge → backend sign.
async fn handle_sign(
    env: &Envelope,
    server_name: &str,
    shed_name: &str,
    gate: &Arc<dyn ApprovalGate>,
    backend: &Arc<dyn SshBackend>,
) -> (serde_json::Value, Option<AuditEntry>) {
    // 1. Decode the sign request (a wrong-typed field → invalid sign request; no audit).
    let req: SignRequestPayload = match serde_json::from_value(env.payload.clone().unwrap_or_default()) {
        Ok(r) => r,
        Err(_) => return (ssh_error("invalid sign request", SSH_CODE_INTERNAL), None),
    };

    // 2. Approval gate FIRST (deny-all default fails closed). The reason shown to the
    //    app is the fixed "SSH sign request" (Go's desktopGate reason) — NOT the key
    //    type, which isn't parsed until after the gate.
    let outcome = gate
        .approve(
            NS_SSH_AGENT,
            "sign",
            server_name,
            shed_name,
            "SSH sign request",
        )
        .await;
    let approval = gate.method().to_string();
    if !outcome.approved {
        // Deny audit: result=denied, approval + decided_by/scope/ttl; NO detail, NO
        // code, NO reason (ssh_handler.go's deny path sets none of those).
        let entry = sign_audit(server_name, shed_name, "denied", "", approval, &outcome);
        return (
            ssh_error("approval denied", SSH_CODE_SIGN_FAILED),
            Some(entry),
        );
    }

    // 3. Decode + parse the public key (parse-error paths do NOT audit).
    let pub_bytes = match decode_b64_lenient(&req.public_key) {
        Ok(b) => b,
        Err(_) => {
            return (
                ssh_error("invalid public key encoding", SSH_CODE_INTERNAL),
                None,
            )
        }
    };
    // ssh_key::PublicKey::from_bytes == Go's ssh.ParsePublicKey (validates + rejects
    // trailing bytes, so `pub_bytes` is the canonical marshaled form — the same bytes
    // Go's keysEqual compares against the loaded key).
    let pubkey = match ssh_key::public::PublicKey::from_bytes(&pub_bytes) {
        Ok(p) => p,
        Err(_) => {
            return (
                ssh_error("invalid public key", SSH_CODE_KEY_NOT_FOUND),
                None,
            )
        }
    };
    let key_type = pubkey.algorithm().as_str().to_string();

    // 4. Decode the challenge data.
    let data = match decode_b64_lenient(&req.data) {
        Ok(d) => d,
        Err(_) => {
            return (
                ssh_error("invalid challenge data encoding", SSH_CODE_INTERNAL),
                None,
            )
        }
    };

    // 5. Sign. Any backend error → {sign operation failed, SIGN_FAILED} + error audit
    //    (detail = key type). Success → SSHSignResponse + ok audit (detail = key type).
    match backend.sign(&pub_bytes, &data, req.flags) {
        Ok(sig) => {
            let resp = SshSignResponse {
                format: sig.format,
                blob: base64::engine::general_purpose::STANDARD.encode(&sig.blob),
                rest: String::new(),
            };
            let payload = to_payload(&resp);
            let entry = sign_audit(server_name, shed_name, "ok", &key_type, approval, &outcome);
            (payload, Some(entry))
        }
        Err(_) => {
            let entry = sign_audit(
                server_name,
                shed_name,
                "error",
                &key_type,
                approval,
                &outcome,
            );
            (
                ssh_error("sign operation failed", SSH_CODE_SIGN_FAILED),
                Some(entry),
            )
        }
    }
}

// ---------------------------------------------------------------------------
// AWS credential handler (a faithful port of aws_handler.go) — the second bus
// namespace. Uses its own per-namespace gate + the AWS protocol error codes, and the
// LogEntry audit form (approval + decided_by/scope/ttl) — unlike the ssh positional
// `list` audit.
// ---------------------------------------------------------------------------

/// AWS response payload types (serde mirrors of `internal/ext/protocol/aws.go`). Built
/// as typed structs (not ad-hoc `json!`) so the tag names are pinned in one place; the
/// `aws_payload_tag_names_match_protocol` test asserts they equal the Go json tags.
/// `expiration`/`cached_until` use `skip_serializing_if` = Go's `omitempty`.
#[derive(Serialize)]
struct AwsCredentialsResponse {
    access_key_id: String,
    secret_access_key: String,
    session_token: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    expiration: Option<String>,
}

/// `AWSPingResponse` — the `ping` op's payload.
#[derive(Serialize)]
struct AwsPingResponse {
    status: String,
}

/// `AWSStatusResponse` — the `status` op's payload. `cached_until` is omitted (Go
/// `omitempty`) when there is no known expiry.
#[derive(Serialize)]
struct AwsStatusResponse {
    connected: bool,
    role: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    cached_until: Option<String>,
}

/// `AWSErrorResponse` — an `{error, code}` error payload.
#[derive(Serialize)]
struct AwsErrorResponse {
    error: String,
    code: String,
}

/// Build an `{error, code}` AWS error payload (`AWSErrorResponse`).
fn aws_error(msg: &str, code: &str) -> serde_json::Value {
    to_payload(&AwsErrorResponse {
        error: msg.to_string(),
        code: code.to_string(),
    })
}

/// A gated AWS audit entry — the LogEntry form (unlike the ssh positional `list`
/// audit). The fixed aws-credentials ns + `get_credentials` op, the given result/detail,
/// the approval method, and the gate outcome's decided_by/scope/ttl. NO `code`/`reason`
/// (aws_handler.go sets neither on ANY get_credentials audit); `detail` is empty on the
/// deny path, `err.Error()` on error, and `awsExpiryDetail` on ok.
fn aws_audit(
    server_name: &str,
    shed_name: &str,
    result: &str,
    detail: &str,
    approval: String,
    outcome: &crate::approval::ApprovalOutcome,
) -> AuditEntry {
    AuditEntry {
        server: server_name.to_string(),
        shed: shed_name.to_string(),
        ns: NS_AWS_CREDENTIALS.to_string(),
        op: AWS_OP_GET_CREDENTIALS.to_string(),
        result: result.to_string(),
        detail: detail.to_string(),
        approval,
        decided_by: outcome.decided_by.clone(),
        scope: outcome.scope.clone().unwrap_or_default(),
        ttl: outcome.ttl.clone().unwrap_or_default(),
        ..Default::default()
    }
}

/// Answer one inbound aws-credentials request, mirroring `aws_handler.go:handleMessage`'s
/// dispatch. `get_credentials` runs the gated flow (approval gate → backend → response +
/// audit); `ping` → `{"status":"ok"}`; `status` → `{connected, role, cached_until?}` (no
/// audit); an unknown op → Go's exact `unknown operation: <op>` `INTERNAL_ERROR`; a
/// non-object/non-null payload → `{invalid payload, INTERNAL_ERROR}` (the shared
/// [`parse_operation`], but with AWS codes). The reply plumbing (mint response, echo
/// `shed`, POST, warn-on-failure) + audit-after-response order match the ssh path.
async fn handle_aws_bus_message(
    client: &BusClient,
    namespace: &str,
    env: &Envelope,
    shutdown: &watch::Receiver<bool>,
    aws: &AwsHandlers,
    audit: &Arc<dyn AuditSink>,
    server_name: &str,
) {
    let (payload, audit_entry) = aws_dispatch(env, aws, server_name).await;
    // Shared reply-plumbing + audit tail (`respond_and_audit`). Only get_credentials
    // audits; ping/status/unknown/invalid leave `audit_entry` None. `what` preserves
    // the aws warn message (`failed to send aws response: ...`).
    respond_and_audit(
        client,
        namespace,
        env,
        shutdown,
        audit,
        payload,
        audit_entry,
        "aws response",
    )
    .await;
}

/// Compute the aws-credentials response payload + optional audit entry for one request,
/// mirroring `aws_handler.go:handleMessage`'s op dispatch (the network-free core of
/// [`handle_aws_bus_message`], so unit tests can inspect the payload + audit directly).
async fn aws_dispatch(
    env: &Envelope,
    aws: &AwsHandlers,
    server_name: &str,
) -> (serde_json::Value, Option<AuditEntry>) {
    let shed_name = env.shed.as_ref().map(|s| s.name.as_str()).unwrap_or("");
    match parse_operation(&env.payload) {
        Err(()) => (aws_error("invalid payload", AWS_CODE_INTERNAL), None),
        Ok(op) => match op.as_str() {
            "get_credentials" => {
                handle_aws_get_credentials(server_name, shed_name, &aws.gate, &aws.backend).await
            }
            "ping" => (
                to_payload(&AwsPingResponse {
                    status: "ok".to_string(),
                }),
                None,
            ),
            "status" => (
                handle_aws_status(server_name, shed_name, &aws.backend),
                None,
            ),
            other => (
                aws_error(&format!("unknown operation: {other}"), AWS_CODE_INTERNAL),
                None,
            ),
        },
    }
}

/// The gated `get_credentials` flow — a faithful port of
/// `aws_handler.go:handleGetCredentials`. ORDER matches Go: approval gate FIRST
/// (deny-all fails closed), THEN the backend vend. Returns the response payload + the
/// audit entry (all three outcomes — deny/error/ok — audit; the parse-error paths in
/// [`handle_aws_bus_message`] do not).
async fn handle_aws_get_credentials(
    server_name: &str,
    shed_name: &str,
    gate: &Arc<dyn ApprovalGate>,
    backend: &Arc<dyn AwsBackend>,
) -> (serde_json::Value, Option<AuditEntry>) {
    let outcome = gate
        .approve(
            NS_AWS_CREDENTIALS,
            AWS_OP_GET_CREDENTIALS,
            server_name,
            shed_name,
            "AWS credentials request",
        )
        .await;
    let approval = gate.method().to_string();
    if !outcome.approved {
        // Deny audit: result=denied, approval + decided_by/scope/ttl; NO detail, NO
        // code, NO reason (aws_handler.go's deny path sets none of those).
        let entry = aws_audit(server_name, shed_name, "denied", "", approval, &outcome);
        return (
            aws_error("approval denied", AWS_CODE_ASSUME_ROLE_FAILED),
            Some(entry),
        );
    }

    match backend.get_credentials(server_name, shed_name).await {
        Err(e) => {
            // Error audit: result=error, detail=err.Error(), approval + outcome; NO code.
            let entry = aws_audit(server_name, shed_name, "error", &e, approval, &outcome);
            (
                aws_error("credential request failed", AWS_CODE_ASSUME_ROLE_FAILED),
                Some(entry),
            )
        }
        Ok(creds) => {
            // expiration OMITTED when None (Go's zero time.Time → omitempty), else the
            // literal-Z UTC render (Go's Format("2006-01-02T15:04:05Z")).
            let resp = AwsCredentialsResponse {
                access_key_id: creds.access_key_id,
                secret_access_key: creds.secret_access_key,
                session_token: creds.session_token,
                expiration: creds.expiration.map(aws_literal_z),
            };
            let detail = aws_expiry_detail(creds.expiration);
            let entry = aws_audit(server_name, shed_name, "ok", &detail, approval, &outcome);
            (to_payload(&resp), Some(entry))
        }
    }
}

/// The `status` op — a faithful port of `aws_handler.go:handleStatus`:
/// `{connected:true, role, cached_until?}`. NO audit (`backend.status` never errors;
/// `cached_until` renders literal-Z when Some, else the field is omitted).
fn handle_aws_status(
    server_name: &str,
    shed_name: &str,
    backend: &Arc<dyn AwsBackend>,
) -> serde_json::Value {
    let (role, cached_until) = backend.status(server_name, shed_name);
    to_payload(&AwsStatusResponse {
        connected: true,
        role,
        cached_until: cached_until.map(aws_literal_z),
    })
}

// ---------------------------------------------------------------------------
// Docker credential handler (a faithful port of docker_handler.go) — the third bus
// namespace. Uses its own per-namespace gate + the Docker protocol error codes. The
// get audit is a LogEntry form that — unlike ssh/aws — SETS code+reason and splits
// `result` into anonymous-vs-error (the CREDENTIALS_NOT_FOUND → anonymous-pull case);
// the list audit is the ssh-style positional form (success only, `approval:"none"`).
// ---------------------------------------------------------------------------

/// Docker response payload types (serde mirrors of `internal/ext/protocol/docker.go`).
/// Built as typed structs (not ad-hoc `json!`) so the tag names are pinned in one place;
/// `docker_payload_tag_names_match_protocol` asserts they equal the Go json tags.
#[derive(Serialize)]
struct DockerGetResponse {
    server_url: String,
    username: String,
    secret: String,
}

/// `DockerListResponse` — the `list` op's payload. `registries` is a registry→username
/// map (Go `map[string]string`; a `BTreeMap` here so the wire is deterministic).
#[derive(Serialize)]
struct DockerListResponse {
    registries: BTreeMap<String, String>,
}

/// `DockerPingResponse` — the `ping` op's payload.
#[derive(Serialize)]
struct DockerPingResponse {
    status: String,
}

/// `DockerStatusResponse` — the `status` op's payload.
#[derive(Serialize)]
struct DockerStatusResponse {
    connected: bool,
    allow_all: bool,
    registry_count: usize,
}

/// `DockerErrorResponse` — an `{error, code}` error payload.
#[derive(Serialize)]
struct DockerErrorResponse {
    error: String,
    code: String,
}

/// The `get` request payload (`DockerGetRequest`). `operation` is already dispatched on;
/// only `server_url` is needed. `#[serde(default)]` so an absent field zero-fills (Go
/// `json.Unmarshal`); a wrong-typed `server_url` fails the decode (Go too).
#[derive(Deserialize)]
struct DockerGetRequestPayload {
    #[serde(default)]
    server_url: String,
}

/// Build an `{error, code}` Docker error payload (`DockerErrorResponse`).
fn docker_error(msg: &str, code: &str) -> serde_json::Value {
    to_payload(&DockerErrorResponse {
        error: msg.to_string(),
        code: code.to_string(),
    })
}

/// A Docker `get` audit entry — the LogEntry form carrying the gate outcome AND
/// `code`/`reason` (unlike ssh/aws get audits, which set neither). Fixed
/// docker-credentials ns + `get` op; `detail` is always the requested `server_url`; a
/// `code`/`reason` of `""` is omitted by the sink (Go omitempty), so the ok path passes
/// `""` for both. Mirrors `docker_handler.go`'s three `LogEntry` calls.
#[allow(clippy::too_many_arguments)]
fn docker_get_audit(
    server_name: &str,
    shed_name: &str,
    result: &str,
    detail: &str,
    code: &str,
    reason: &str,
    approval: String,
    outcome: &crate::approval::ApprovalOutcome,
) -> AuditEntry {
    AuditEntry {
        server: server_name.to_string(),
        shed: shed_name.to_string(),
        ns: NS_DOCKER_CREDENTIALS.to_string(),
        op: DOCKER_OP_GET.to_string(),
        result: result.to_string(),
        detail: detail.to_string(),
        code: code.to_string(),
        reason: reason.to_string(),
        approval,
        decided_by: outcome.decided_by.clone(),
        scope: outcome.scope.clone().unwrap_or_default(),
        ttl: outcome.ttl.clone().unwrap_or_default(),
        ..Default::default()
    }
}

/// A Docker `list` audit entry — Go's positional `AuditLogger.Log` form (like ssh's
/// `list`, NOT the LogEntry form): fixed docker-credentials ns + `list` op, the given
/// result/detail, `approval:"none"`, and NO decided_by/scope/ttl/code/reason. `list`
/// audits ONLY on success (the error path returns audit-silent).
fn docker_list_audit(server_name: &str, shed_name: &str, detail: &str) -> AuditEntry {
    AuditEntry {
        server: server_name.to_string(),
        shed: shed_name.to_string(),
        ns: NS_DOCKER_CREDENTIALS.to_string(),
        op: DOCKER_OP_LIST.to_string(),
        result: "ok".to_string(),
        detail: detail.to_string(),
        approval: "none".to_string(),
        ..Default::default()
    }
}

/// Answer one inbound docker-credentials request, mirroring `docker_handler.go:handleMessage`'s
/// dispatch. `get` runs the gated flow (approval gate → backend → response + audit);
/// `list` → the UNGATED registry listing (success audits positionally, error is
/// audit-silent); `ping` → `{"status":"ok"}`; `status` → `{connected, allow_all,
/// registry_count}`; an unknown op → Go's exact `unknown operation: <op>` `INTERNAL_ERROR`;
/// a non-object/non-null payload → `{invalid payload, INTERNAL_ERROR}` (the shared
/// [`parse_operation`], but with Docker codes). The reply plumbing + audit-after-response
/// order match the ssh/aws paths (`respond_and_audit`).
async fn handle_docker_bus_message(
    client: &BusClient,
    namespace: &str,
    env: &Envelope,
    shutdown: &watch::Receiver<bool>,
    docker: &DockerHandlers,
    audit: &Arc<dyn AuditSink>,
    server_name: &str,
) {
    let (payload, audit_entry) = docker_dispatch(env, docker, server_name).await;
    respond_and_audit(
        client,
        namespace,
        env,
        shutdown,
        audit,
        payload,
        audit_entry,
        "docker response",
    )
    .await;
}

/// Compute the docker-credentials response payload + optional audit entry for one request
/// (the network-free core of [`handle_docker_bus_message`], so unit tests inspect the
/// payload + audit directly). `get`/`list` audit per Go; `ping`/`status`/unknown/invalid
/// do not.
async fn docker_dispatch(
    env: &Envelope,
    docker: &DockerHandlers,
    server_name: &str,
) -> (serde_json::Value, Option<AuditEntry>) {
    let shed_name = env.shed.as_ref().map(|s| s.name.as_str()).unwrap_or("");
    match parse_operation(&env.payload) {
        Err(()) => (docker_error("invalid payload", DOCKER_CODE_INTERNAL), None),
        Ok(op) => match op.as_str() {
            DOCKER_OP_GET => {
                handle_docker_get(env, server_name, shed_name, &docker.gate, &docker.backend).await
            }
            DOCKER_OP_LIST => {
                handle_docker_list(server_name, shed_name, &docker.backend).await
            }
            "ping" => (
                to_payload(&DockerPingResponse {
                    status: "ok".to_string(),
                }),
                None,
            ),
            "status" => (
                handle_docker_status(server_name, shed_name, &docker.backend),
                None,
            ),
            other => (
                docker_error(&format!("unknown operation: {other}"), DOCKER_CODE_INTERNAL),
                None,
            ),
        },
    }
}

/// The gated `get` flow — a faithful port of `docker_handler.go:handleGet`. ORDER matches
/// Go: parse request → **approval gate** → backend. On gate-deny the guest gets
/// `{approval denied, REGISTRY_NOT_ALLOWED}` while the audit carries `code:APPROVAL_DENIED,
/// result:denied` (the two-code disambiguation). On a backend error the guest gets the
/// original code (`DockerCredError.code`, or `INTERNAL_ERROR` when empty) and the audit
/// splits `result` into `anonymous` (CREDENTIALS_NOT_FOUND — the guest pulls anonymously)
/// vs `error`, with `code`+`reason`(err msg)+`detail`(server_url). On success →
/// `DockerGetResponse` + an ok audit (detail = server_url, NO code/reason).
async fn handle_docker_get(
    env: &Envelope,
    server_name: &str,
    shed_name: &str,
    gate: &Arc<dyn ApprovalGate>,
    backend: &Arc<dyn DockerBackend>,
) -> (serde_json::Value, Option<AuditEntry>) {
    // Parse the get request (a wrong-typed `server_url` → invalid get request; no audit).
    let req: DockerGetRequestPayload =
        match serde_json::from_value(env.payload.clone().unwrap_or_default()) {
            Ok(r) => r,
            Err(_) => return (docker_error("invalid get request", DOCKER_CODE_INTERNAL), None),
        };

    // Approval gate FIRST (deny-all fails closed). The reason names the registry so the
    // desktop approval card shows what is being requested (Go's `fmt.Sprintf`).
    let outcome = gate
        .approve(
            NS_DOCKER_CREDENTIALS,
            DOCKER_OP_GET,
            server_name,
            shed_name,
            &format!("Docker credentials for {}", req.server_url),
        )
        .await;
    let approval = gate.method().to_string();
    if !outcome.approved {
        // Deny audit: code=APPROVAL_DENIED (audit-only), reason=the gate deny reason,
        // detail=server_url + the outcome. The guest still receives REGISTRY_NOT_ALLOWED.
        let entry = docker_get_audit(
            server_name,
            shed_name,
            "denied",
            &req.server_url,
            DOCKER_AUDIT_CODE_APPROVAL_DENIED,
            &outcome.reason,
            approval,
            &outcome,
        );
        return (
            docker_error("approval denied", DOCKER_CODE_NOT_ALLOWED),
            Some(entry),
        );
    }

    match backend
        .get_credentials(server_name, shed_name, &req.server_url)
        .await
    {
        Ok(cred) => {
            let resp = DockerGetResponse {
                server_url: cred.server_url,
                username: cred.username,
                secret: cred.secret,
            };
            // ok audit: detail=server_url, NO code/reason (Go passes neither).
            let entry = docker_get_audit(
                server_name,
                shed_name,
                "ok",
                &req.server_url,
                "",
                "",
                approval,
                &outcome,
            );
            (to_payload(&resp), Some(entry))
        }
        Err(e) => {
            // An empty backend code maps to INTERNAL_ERROR (Go's bare-`fmt.Errorf` cases).
            let code = if e.code.is_empty() {
                DOCKER_CODE_INTERNAL
            } else {
                e.code.as_str()
            };
            // CREDENTIALS_NOT_FOUND for an allowed registry is an anonymous pull, not a
            // failure → audit `anonymous`; everything else (deny, helper fault) → `error`.
            let result = if code == DOCKER_CODE_NOT_FOUND {
                DOCKER_AUDIT_RESULT_ANONYMOUS
            } else {
                "error"
            };
            // The guest receives the original code; the audit enriches with code+reason.
            let entry = docker_get_audit(
                server_name,
                shed_name,
                result,
                &req.server_url,
                code,
                &e.msg,
                approval,
                &outcome,
            );
            (docker_error("credential request failed", code), Some(entry))
        }
    }
}

/// The `list` op — a faithful port of `docker_handler.go:handleList`. UNGATED (Go never
/// invokes the approval gate on `list`). Success → `DockerListResponse{registries}` + a
/// positional audit (`count:N`, `approval:none`). A backend error → `{list failed,
/// INTERNAL_ERROR}` and returns with **NO audit** (Go returns early; only success audits).
async fn handle_docker_list(
    server_name: &str,
    shed_name: &str,
    backend: &Arc<dyn DockerBackend>,
) -> (serde_json::Value, Option<AuditEntry>) {
    match backend.list_credentials(server_name, shed_name).await {
        Ok(registries) => {
            let count = registries.len();
            let payload = to_payload(&DockerListResponse { registries });
            let entry = docker_list_audit(server_name, shed_name, &format!("count:{count}"));
            (payload, Some(entry))
        }
        // Error path: audit-silent (matches Go's early return, no LogEntry).
        Err(_) => (docker_error("list failed", DOCKER_CODE_INTERNAL), None),
    }
}

/// The `status` op — a faithful port of `docker_handler.go:handleStatus`:
/// `{connected:true, allow_all, registry_count}`. `Status` never errors. NO audit.
fn handle_docker_status(
    server_name: &str,
    shed_name: &str,
    backend: &Arc<dyn DockerBackend>,
) -> serde_json::Value {
    let (allow_all, registry_count) = backend.status(server_name, shed_name);
    to_payload(&DockerStatusResponse {
        connected: true,
        allow_all,
        registry_count,
    })
}

/// The credential namespaces (re-exported from `config` for callers wiring the
/// bus). `ssh-agent` is always subscribed; `aws-credentials` when configured;
/// `docker-credentials` in the common case (even unconfigured).
pub const BUS_NAMESPACES: [&str; 3] = [NS_SSH_AGENT, NS_AWS_CREDENTIALS, NS_DOCKER_CREDENTIALS];

#[cfg(test)]
mod tests {
    use super::*;
    use httpmock::prelude::*;
    use std::sync::atomic::AtomicUsize;

    fn silent_log() -> Arc<dyn BusLog> {
        struct Silent;
        impl BusLog for Silent {
            fn info(&self, _: &str) {}
            fn warn(&self, _: &str) {}
            fn debug(&self, _: &str) {}
            fn error(&self, _: &str) {}
        }
        Arc::new(Silent)
    }

    /// A collecting log that counts messages per level, so the loud-then-quiet
    /// reconnect logging can be asserted (mirrors Go's TestReconnectLogDedup).
    #[derive(Default)]
    struct CountingLog {
        warns: AtomicUsize,
        debugs: AtomicUsize,
        errors: AtomicUsize,
    }
    impl BusLog for CountingLog {
        fn info(&self, _: &str) {}
        fn warn(&self, _: &str) {
            self.warns.fetch_add(1, Ordering::SeqCst);
        }
        fn debug(&self, _: &str) {
            self.debugs.fetch_add(1, Ordering::SeqCst);
        }
        fn error(&self, _: &str) {
            self.errors.fetch_add(1, Ordering::SeqCst);
        }
    }

    /// A token provider returning `tok-1`, `tok-2`, ... ; `invalidate` advances the
    /// sequence and counts calls (mirrors hostclient_test.go's fakeTokenProvider).
    struct SeqTokenProvider {
        idx: Mutex<usize>,
        invalidated: AtomicUsize,
    }
    impl SeqTokenProvider {
        fn new() -> Arc<Self> {
            Arc::new(Self {
                idx: Mutex::new(1),
                invalidated: AtomicUsize::new(0),
            })
        }
        fn invalidated(&self) -> usize {
            self.invalidated.load(Ordering::SeqCst)
        }
    }
    #[async_trait::async_trait]
    impl TokenProvider for SeqTokenProvider {
        async fn token(&self) -> Result<String, BusError> {
            Ok(format!("tok-{}", *self.idx.lock().unwrap()))
        }
        fn invalidate(&self) {
            self.invalidated.fetch_add(1, Ordering::SeqCst);
            *self.idx.lock().unwrap() += 1;
        }
    }

    fn open_client(base_url: &str) -> BusClient {
        BusClient::new(
            base_url.to_string(),
            String::new(),
            None,
            None,
            silent_log(),
        )
        .unwrap()
    }

    fn shutdown_pair() -> (watch::Sender<bool>, watch::Receiver<bool>) {
        watch::channel(false)
    }

    /// A shutdown receiver that never fires. The sender is intentionally leaked so
    /// `wait_for` blocks forever (a dropped sender would resolve it immediately,
    /// making the network await abort as if shutdown had fired). For tests whose
    /// respond/handle path must actually reach the mock server.
    fn never_shutdown() -> watch::Receiver<bool> {
        let (tx, rx) = watch::channel(false);
        let _ = Box::leak(Box::new(tx));
        rx
    }

    // ---- sign-flow test doubles ----

    use crate::approval::{ApproveAllGate, DenyAllGate};
    use crate::ssh_backend::{SshKeyInfo, SshSignature};

    fn approve_gate() -> Arc<dyn ApprovalGate> {
        Arc::new(ApproveAllGate)
    }
    fn deny_gate() -> Arc<dyn ApprovalGate> {
        Arc::new(DenyAllGate)
    }

    /// Bundle the three seams into a `BusHandlers` with an empty (single-server)
    /// `server_name`, for the `handle_bus_message` dispatch tests.
    fn test_handlers(
        gate: Arc<dyn ApprovalGate>,
        audit: Arc<dyn AuditSink>,
        backend: Arc<dyn SshBackend>,
    ) -> BusHandlers {
        BusHandlers {
            gate,
            audit,
            backend,
            server_name: String::new(),
            aws: None,
            docker: None,
        }
    }

    /// An audit sink that records every entry, for asserting the sign audit shape.
    #[derive(Default)]
    struct CollectingAudit {
        entries: Mutex<Vec<AuditEntry>>,
    }
    impl AuditSink for CollectingAudit {
        fn log(&self, entry: AuditEntry) {
            self.entries.lock().unwrap().push(entry);
        }
    }
    fn noop_audit() -> Arc<dyn AuditSink> {
        Arc::new(CollectingAudit::default())
    }

    /// A backend that signs `data` (returns `canned` bytes) only for the one pubkey it
    /// was built with; any other pubkey → `key not found` (like the real backend).
    struct StubBackend {
        pubkey: Vec<u8>,
        result: Result<SshSignature, String>,
    }
    impl SshBackend for StubBackend {
        fn list(&self) -> Result<Vec<SshKeyInfo>, String> {
            Ok(Vec::new())
        }
        fn sign(
            &self,
            public_key: &[u8],
            _data: &[u8],
            _flags: u32,
        ) -> Result<SshSignature, String> {
            if public_key == self.pubkey {
                self.result.clone()
            } else {
                Err("key not found".to_string())
            }
        }
        fn mode(&self) -> &str {
            "local-keys"
        }
    }
    fn empty_backend() -> Arc<dyn SshBackend> {
        Arc::new(StubBackend {
            pubkey: Vec::new(),
            result: Err("key not found".to_string()),
        })
    }

    /// A valid ed25519 SSH-wire marshaled public key (Go `ssh.PublicKey.Marshal()`),
    /// built from a fixed seed so `PublicKey::from_bytes` accepts it.
    fn fixed_ed25519_pub() -> Vec<u8> {
        use ssh_key::private::{Ed25519Keypair, Ed25519PrivateKey, KeypairData, PrivateKey};
        use ssh_key::public::Ed25519PublicKey;
        let seed = [3u8; 32];
        let verifying = ed25519_dalek::SigningKey::from_bytes(&seed).verifying_key();
        let keypair = Ed25519Keypair {
            public: Ed25519PublicKey(verifying.to_bytes()),
            private: Ed25519PrivateKey::from_bytes(&seed),
        };
        PrivateKey::new(KeypairData::Ed25519(keypair), "t")
            .unwrap()
            .public_key()
            .to_bytes()
            .unwrap()
    }

    fn b64(bytes: &[u8]) -> String {
        base64::engine::general_purpose::STANDARD.encode(bytes)
    }

    /// Build a `sign` request Envelope with the given base64 public_key/data + flags.
    fn sign_env(public_key: &str, data: &str, flags: u32) -> Envelope {
        Envelope {
            id: "sign-1".into(),
            namespace: "ssh-agent".into(),
            msg_type: "request".into(),
            in_reply_to: String::new(),
            is_final: true,
            timestamp: "t".into(),
            payload: Some(serde_json::json!({
                "operation": "sign",
                "public_key": public_key,
                "data": data,
                "flags": flags,
            })),
            shed: Some(ShedInfo {
                name: "web".into(),
                backend: "vz".into(),
                server: "mini2".into(),
            }),
        }
    }

    // ---- base64 leniency (Go parity) ----

    #[test]
    fn decode_b64_lenient_skips_crlf_like_go() {
        let raw = base64::engine::general_purpose::STANDARD.encode(b"hello world data");
        // Wrap with a bare LF and a CRLF (line-wrapped base64) — Go decodes this fine.
        let wrapped = format!("{}\n{}\r\n{}", &raw[..8], &raw[8..12], &raw[12..]);
        assert_eq!(decode_b64_lenient(&wrapped).unwrap(), b"hello world data");
        // The un-wrapped form still decodes.
        assert_eq!(decode_b64_lenient(&raw).unwrap(), b"hello world data");
        // A genuinely invalid char (not CR/LF) is still an error (Go doesn't skip it).
        assert!(decode_b64_lenient("not valid base64 !!!").is_err());
    }

    // ---- Envelope shape / new_response ----

    #[test]
    fn new_response_matches_go_wire_shape() {
        let env =
            Envelope::new_response("req-123", "ssh-agent", serde_json::json!({"status":"ok"}));
        assert_eq!(env.msg_type, "response");
        assert!(env.is_final);
        assert_eq!(env.in_reply_to, "req-123");
        assert_eq!(env.namespace, "ssh-agent");
        assert_eq!(env.id.len(), 36); // a UUID string
        assert!(!env.timestamp.is_empty());
        assert!(env.shed.is_none());

        // The serialized JSON must have Go's exact top-level key order + no `shed`.
        let json = serde_json::to_string(&env).unwrap();
        let order = top_level_key_order(&json);
        assert_eq!(
            order,
            vec![
                "id",
                "namespace",
                "type",
                "in_reply_to",
                "final",
                "timestamp",
                "payload"
            ],
            "response envelope key order must match sdk/envelope.go NewResponse marshal"
        );
        // Re-parse to confirm values + payload shape.
        let v: serde_json::Value = serde_json::from_str(&json).unwrap();
        assert_eq!(v["type"], "response");
        assert_eq!(v["final"], true);
        assert_eq!(v["in_reply_to"], "req-123");
        assert_eq!(v["payload"], serde_json::json!({"status":"ok"}));
        assert!(v.get("shed").is_none());
    }

    #[test]
    fn response_carries_shed_when_set() {
        let mut env = Envelope::new_response("r", "ssh-agent", serde_json::json!({"status":"ok"}));
        env.shed = Some(ShedInfo {
            name: "folio".into(),
            backend: "vz".into(),
            server: "mini2".into(),
        });
        let v: serde_json::Value = serde_json::to_value(&env).unwrap();
        assert_eq!(v["shed"]["name"], "folio");
        assert_eq!(v["shed"]["backend"], "vz");
        assert_eq!(v["shed"]["server"], "mini2");
    }

    #[test]
    fn request_envelope_round_trips_from_go_shape() {
        // A request envelope as shed-server marshals it (in_reply_to omitted).
        let wire = r#"{"id":"abc","namespace":"ssh-agent","type":"request","final":true,"timestamp":"2026-07-09T00:00:00Z","payload":{"operation":"ping"},"shed":{"name":"folio","backend":"vz","server":"mini2"}}"#;
        let env: Envelope = serde_json::from_str(wire).unwrap();
        assert_eq!(env.msg_type, "request");
        assert_eq!(env.namespace, "ssh-agent");
        assert_eq!(env.in_reply_to, ""); // omitted → empty
        assert_eq!(env.operation(), Some("ping"));
        assert_eq!(env.shed.as_ref().unwrap().backend, "vz");
    }

    #[test]
    fn envelope_tolerates_absent_payload_and_shed() {
        let wire = r#"{"id":"x","namespace":"ns","type":"event","final":false,"timestamp":"t"}"#;
        let env: Envelope = serde_json::from_str(wire).unwrap();
        // An OMITTED payload is `None` (distinct from an explicit `null` →
        // `Some(Null)`): Go's nil json.RawMessage fails the handler unmarshal
        // (`invalid payload`) while explicit null parses to a zero op.
        assert!(env.payload.is_none());
        assert!(env.shed.is_none());
        assert_eq!(env.operation(), None);
        // Re-serialized, an absent payload marshals as `null` (nil json.RawMessage).
        let v: serde_json::Value = serde_json::to_value(&env).unwrap();
        assert!(v["payload"].is_null());

        let explicit_null = r#"{"id":"x","namespace":"ns","type":"event","final":false,"timestamp":"t","payload":null}"#;
        let env2: Envelope = serde_json::from_str(explicit_null).unwrap();
        assert_eq!(env2.payload, Some(serde_json::Value::Null));
    }

    #[test]
    fn parse_operation_distinguishes_absent_from_null_payload() {
        // Omitted payload field → unmarshal error in Go → invalid payload.
        assert_eq!(parse_operation(&None), Err(()));
        // Explicit null payload → zero operation → "unknown operation: ".
        assert_eq!(
            parse_operation(&Some(serde_json::Value::Null)),
            Ok(String::new())
        );
    }

    // Extract the top-level object key order from a serialized JSON string.
    fn top_level_key_order(json: &str) -> Vec<String> {
        let mut keys = Vec::new();
        let bytes = json.as_bytes();
        let mut depth = 0usize;
        let mut i = 0;
        while i < bytes.len() {
            match bytes[i] {
                b'{' | b'[' => depth += 1,
                b'}' | b']' => depth -= 1,
                b'"' if depth == 1 => {
                    // Read the string; if followed by ':', it's a key.
                    let start = i + 1;
                    let mut j = start;
                    while j < bytes.len() && bytes[j] != b'"' {
                        if bytes[j] == b'\\' {
                            j += 1;
                        }
                        j += 1;
                    }
                    let s = &json[start..j];
                    let mut k = j + 1;
                    while k < bytes.len() && (bytes[k] as char).is_whitespace() {
                        k += 1;
                    }
                    if k < bytes.len() && bytes[k] == b':' {
                        keys.push(s.to_string());
                    }
                    i = j;
                }
                _ => {}
            }
            i += 1;
        }
        keys
    }

    // ---- TLS pin fail-closed ----

    #[test]
    fn pin_on_non_https_is_config_error() {
        let r = BusClient::new(
            "http://mini2:8080".to_string(),
            String::new(),
            None,
            Some("sha256:aa".into()),
            silent_log(),
        );
        assert!(matches!(r, Err(BusError::Config(_))));
    }

    #[test]
    fn pin_on_https_builds_ok() {
        let r = BusClient::new(
            "https://mini2:8443".to_string(),
            String::new(),
            None,
            Some("sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824".into()),
            silent_log(),
        );
        assert!(
            r.is_ok(),
            "a pin on an https URL must build a pinned client"
        );
    }

    #[test]
    fn empty_pin_is_ignored_on_http() {
        // An empty pin string is treated as "no pin" (fail-closed only fires on a
        // real pin), so a plain-http open client still builds.
        let r = BusClient::new(
            "http://mini2:8080".to_string(),
            String::new(),
            None,
            Some(String::new()),
            silent_log(),
        );
        assert!(r.is_ok());
    }

    // ---- Backoff logic (pure) ----

    #[test]
    fn backoff_doubles_and_caps() {
        let doublecap = |b: Duration| (b * 2).min(MAX_BACKOFF);
        let mut b = INITIAL_BACKOFF;
        assert_eq!(b, Duration::from_secs(1));
        b = doublecap(b);
        assert_eq!(b, Duration::from_secs(2));
        for _ in 0..10 {
            b = doublecap(b);
        }
        assert_eq!(b, MAX_BACKOFF); // caps at 30s
    }

    // ---- respond (mirrors hostclient_test.go) ----

    #[tokio::test]
    async fn respond_posts_envelope_and_expects_204() {
        let server = MockServer::start_async().await;
        let m = server
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/plugins/listeners/ssh-agent/respond")
                    .header("content-type", "application/json");
                t.status(204);
            })
            .await;
        let env = Envelope::new_response("req-1", "ssh-agent", serde_json::json!({"status":"ok"}));
        open_client(&server.base_url())
            .respond("ssh-agent", &env, &never_shutdown())
            .await
            .unwrap();
        m.assert_async().await;
    }

    #[tokio::test]
    async fn respond_non_204_is_error() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(POST).path("/api/plugins/listeners/ns/respond");
                t.status(400).body("bad request");
            })
            .await;
        let env = Envelope::new_response("r", "ns", serde_json::Value::Null);
        let err = open_client(&server.base_url())
            .respond("ns", &env, &never_shutdown())
            .await
            .unwrap_err();
        assert!(matches!(err, BusError::BadStatus(400, _)));
    }

    #[tokio::test]
    async fn respond_open_mode_sends_no_auth_header() {
        let server = MockServer::start_async().await;
        // Matches only when NO Authorization header is present.
        let m = server
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/plugins/listeners/ns/respond")
                    .matches(|req| {
                        !req.headers
                            .as_ref()
                            .map(|h| {
                                h.iter()
                                    .any(|(k, _)| k.eq_ignore_ascii_case("authorization"))
                            })
                            .unwrap_or(false)
                    });
                t.status(204);
            })
            .await;
        let env = Envelope::new_response("r", "ns", serde_json::Value::Null);
        open_client(&server.base_url())
            .respond("ns", &env, &never_shutdown())
            .await
            .unwrap();
        m.assert_async().await;
    }

    #[tokio::test]
    async fn respond_refreshes_on_401_then_retries_once() {
        let server = MockServer::start_async().await;
        // Stale token → 401.
        server
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/plugins/listeners/ns/respond")
                    .header("authorization", "Bearer tok-1");
                t.status(401);
            })
            .await;
        // Re-minted token → 204.
        let ok = server
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/plugins/listeners/ns/respond")
                    .header("authorization", "Bearer tok-2");
                t.status(204);
            })
            .await;
        let tp = SeqTokenProvider::new();
        let client = BusClient::new(
            server.base_url(),
            String::new(),
            Some(tp.clone()),
            None,
            silent_log(),
        )
        .unwrap();
        let env = Envelope::new_response("r", "ns", serde_json::Value::Null);
        client.respond("ns", &env, &never_shutdown()).await.unwrap();
        assert_eq!(tp.invalidated(), 1, "exactly one invalidate on the 401");
        ok.assert_async().await;
    }

    #[tokio::test]
    async fn respond_retries_at_most_once_on_persistent_401() {
        let server = MockServer::start_async().await;
        let m = server
            .mock_async(|w, t| {
                w.method(POST).path("/api/plugins/listeners/ns/respond");
                t.status(401); // always 401
            })
            .await;
        let tp = SeqTokenProvider::new();
        let client = BusClient::new(
            server.base_url(),
            String::new(),
            Some(tp.clone()),
            None,
            silent_log(),
        )
        .unwrap();
        let env = Envelope::new_response("r", "ns", serde_json::Value::Null);
        let err = client
            .respond("ns", &env, &never_shutdown())
            .await
            .unwrap_err();
        assert!(matches!(err, BusError::BadStatus(401, _)));
        assert_eq!(
            tp.invalidated(),
            1,
            "at-most-once retry → exactly one invalidate"
        );
        m.assert_hits_async(2).await; // initial + one retry
    }

    /// Bug 1 regression: a server that accepts the POST but never replies must not
    /// pin the daemon — when the shutdown signal fires, `respond` aborts promptly
    /// (mirrors Go threading `ctx` through Respond).
    #[tokio::test]
    async fn respond_aborts_promptly_on_shutdown_against_hung_server() {
        // An in-process TCP server that accepts every connection and holds it open,
        // never sending an HTTP response → `req.send()` stays pending forever.
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            let mut held = Vec::new();
            while let Ok((stream, _)) = listener.accept().await {
                held.push(stream); // hold open, never respond
            }
        });

        let client = open_client(&format!("http://{addr}"));
        let (tx, rx) = shutdown_pair();
        let env = Envelope::new_response("r", "ns", serde_json::Value::Null);
        let respond = tokio::spawn(async move { client.respond("ns", &env, &rx).await });

        // Let the POST connect + block on the (absent) response, then signal shutdown.
        tokio::time::sleep(Duration::from_millis(100)).await;
        let _ = tx.send(true);

        // Without the shutdown-race this would hang until the 2s timeout trips.
        let result = tokio::time::timeout(Duration::from_secs(2), respond)
            .await
            .expect("respond did not abort on shutdown — it hung")
            .unwrap();
        assert!(
            matches!(result, Err(BusError::Transport(_))),
            "respond against a hung server must abort with a transport error on shutdown, got {result:?}"
        );
    }

    // ---- subscribe (mirrors hostclient_test.go) ----

    #[tokio::test]
    async fn subscribe_receives_envelopes() {
        let server = MockServer::start_async().await;
        let sse = "data: {\"id\":\"e1\",\"namespace\":\"ssh-agent\",\"type\":\"request\",\"final\":true,\"timestamp\":\"t\",\"payload\":{\"operation\":\"ping\"}}\n\n\
                   data: {\"id\":\"e2\",\"namespace\":\"ssh-agent\",\"type\":\"request\",\"final\":true,\"timestamp\":\"t\",\"payload\":{\"operation\":\"list\"}}\n\n";
        server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/plugins/listeners/ssh-agent/messages")
                    .header("accept", "text/event-stream");
                t.status(200)
                    .header("content-type", "text/event-stream")
                    .body(sse);
            })
            .await;
        let (_tx, rx) = shutdown_pair();
        let mut sub = open_client(&server.base_url()).subscribe("ssh-agent", rx);
        let e1 = tokio::time::timeout(Duration::from_secs(5), sub.rx.recv())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(e1.id, "e1");
        assert_eq!(e1.operation(), Some("ping"));
        let e2 = tokio::time::timeout(Duration::from_secs(5), sub.rx.recv())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(e2.id, "e2");
        assert_eq!(e2.operation(), Some("list"));
    }

    #[tokio::test]
    async fn subscribe_409_is_terminal_and_rejected() {
        let server = MockServer::start_async().await;
        let m = server
            .mock_async(|w, t| {
                w.method(GET).path("/api/plugins/listeners/ns1/messages");
                t.status(409)
                    .body("namespace \"ns1\" is already registered");
            })
            .await;
        let (_tx, rx) = shutdown_pair();
        let mut sub = open_client(&server.base_url()).subscribe("ns1", rx);
        // The channel must close on its own (no envelope, terminal).
        let closed = tokio::time::timeout(Duration::from_secs(3), sub.rx.recv())
            .await
            .expect("subscribe did not terminate on 409 — it is hot-looping");
        assert!(
            closed.is_none(),
            "no envelope on a 409-rejected subscription"
        );
        // Observably rejected, hit exactly once (no retry).
        assert_eq!(sub.status().state, ConnState::Rejected);
        assert!(!sub.status().last_error.is_empty());
        m.assert_hits_async(1).await;
    }

    #[tokio::test]
    async fn subscribe_reconnects_after_stream_close() {
        // Each connect returns one envelope then the body ends → the loop must
        // reconnect and connect again (server hit more than once).
        let server = MockServer::start_async().await;
        let m = server
            .mock_async(|w, t| {
                w.method(GET).path("/api/plugins/listeners/ssh-agent/messages");
                t.status(200).body(
                    "data: {\"id\":\"e\",\"namespace\":\"ssh-agent\",\"type\":\"request\",\"final\":true,\"timestamp\":\"t\",\"payload\":{}}\n\n",
                );
            })
            .await;
        let client = open_client(&server.base_url())
            .with_test_backoff(Duration::from_millis(5), Duration::from_millis(20));
        let (_tx, rx) = shutdown_pair();
        let mut sub = client.subscribe("ssh-agent", rx);
        // Drain a couple of envelopes across reconnects.
        for _ in 0..2 {
            let e = tokio::time::timeout(Duration::from_secs(5), sub.rx.recv())
                .await
                .unwrap()
                .unwrap();
            assert_eq!(e.id, "e");
        }
        let hits = m.hits_async().await;
        assert!(hits >= 2, "expected a reconnect (>=2 hits), got {hits}");
    }

    #[tokio::test]
    async fn subscribe_401_invalidates_then_reconnects_authenticated() {
        let server = MockServer::start_async().await;
        // Stale token → 401.
        let stale = server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/plugins/listeners/ssh-agent/messages")
                    .header("authorization", "Bearer tok-1");
                t.status(401);
            })
            .await;
        // Re-minted token → 200 SSE with one envelope.
        server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/plugins/listeners/ssh-agent/messages")
                    .header("authorization", "Bearer tok-2");
                t.status(200).body(
                    "data: {\"id\":\"after401\",\"namespace\":\"ssh-agent\",\"type\":\"request\",\"final\":true,\"timestamp\":\"t\",\"payload\":{}}\n\n",
                );
            })
            .await;
        let tp = SeqTokenProvider::new();
        let client = BusClient::new(
            server.base_url(),
            String::new(),
            Some(tp.clone()),
            None,
            silent_log(),
        )
        .unwrap()
        .with_test_backoff(Duration::from_millis(5), Duration::from_millis(20));
        let (_tx, rx) = shutdown_pair();
        let mut sub = client.subscribe("ssh-agent", rx);
        let e = tokio::time::timeout(Duration::from_secs(5), sub.rx.recv())
            .await
            .expect("no envelope after the 401 re-mint")
            .unwrap();
        assert_eq!(e.id, "after401");
        assert!(tp.invalidated() >= 1, "the 401 must invalidate the token");
        stale.assert_hits_async(1).await;
    }

    #[tokio::test]
    async fn subscribe_down_logs_loud_then_quiet() {
        // A server that always 500s → the loop backs off; the WARN fires once,
        // then the quiet DEBUG tier (mirrors hostclient.go's log dedup).
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/plugins/listeners/ssh-agent/messages");
                t.status(500);
            })
            .await;
        let log = Arc::new(CountingLog::default());
        let client = BusClient::new(server.base_url(), String::new(), None, None, log.clone())
            .unwrap()
            .with_test_backoff(Duration::from_millis(5), Duration::from_millis(10));
        let (tx, rx) = shutdown_pair();
        let sub = client.subscribe("ssh-agent", rx);
        // Let several backoff cycles elapse, then stop.
        tokio::time::sleep(Duration::from_millis(80)).await;
        let _ = tx.send(true);
        drop(sub);
        tokio::time::sleep(Duration::from_millis(20)).await;
        assert_eq!(
            log.warns.load(Ordering::SeqCst),
            1,
            "WARN loud exactly once"
        );
        assert!(
            log.debugs.load(Ordering::SeqCst) >= 1,
            "subsequent retries drop to the quiet DEBUG tier"
        );
    }

    #[tokio::test]
    async fn subscribe_shuts_down_cleanly() {
        // A held-open stream must stop promptly on the shutdown signal (the daemon
        // teardown contract).
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/plugins/listeners/ssh-agent/messages");
                // A 200 with an empty body: connects then the body ends immediately;
                // the loop would reconnect, but shutdown stops it.
                t.status(200).body("");
            })
            .await;
        let client = open_client(&server.base_url())
            .with_test_backoff(Duration::from_millis(20), Duration::from_millis(40));
        let (tx, rx) = shutdown_pair();
        let mut sub = client.subscribe("ssh-agent", rx);
        let _ = tx.send(true);
        // The channel closes once the loop observes shutdown.
        let closed = tokio::time::timeout(Duration::from_secs(3), sub.rx.recv())
            .await
            .expect("subscribe did not shut down");
        assert!(closed.is_none());
    }

    // ---- ping responder ----

    #[tokio::test]
    async fn ping_responder_answers_pong() {
        let server = MockServer::start_async().await;
        // The responder POSTs a pong response for the ping.
        let respond = server
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/plugins/listeners/ssh-agent/respond")
                    .matches(|req| {
                        let body = req
                            .body
                            .as_ref()
                            .map(|b| String::from_utf8_lossy(b).to_string())
                            .unwrap_or_default();
                        body.contains("\"type\":\"response\"")
                            && body.contains("\"in_reply_to\":\"ping-req-1\"")
                            && body.contains("\"status\":\"ok\"")
                    });
                t.status(204);
            })
            .await;
        let client = open_client(&server.base_url());
        let req = Envelope {
            id: "ping-req-1".into(),
            namespace: "ssh-agent".into(),
            msg_type: "request".into(),
            in_reply_to: String::new(),
            is_final: true,
            timestamp: "t".into(),
            payload: Some(serde_json::json!({"operation":"ping"})),
            shed: Some(ShedInfo {
                name: "folio".into(),
                backend: "vz".into(),
                server: "mini2".into(),
            }),
        };
        handle_bus_message(
            &client,
            "ssh-agent",
            &req,
            &never_shutdown(),
            &test_handlers(approve_gate(), noop_audit(), empty_backend()),
        )
        .await;
        respond.assert_async().await;
    }

    #[tokio::test]
    async fn unknown_op_exact_string() {
        // A truly-unknown op (`list`/`status`/`sign`/`ping` are all implemented now)
        // gets Go's exact `unknown operation: <op>` INTERNAL_ERROR envelope so the
        // shed's request doesn't hang. (Retargeted from the old `list`-probe test now
        // that `list` is a real op.)
        let server = MockServer::start_async().await;
        let respond = server
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/plugins/listeners/ssh-agent/respond")
                    .matches(|req| {
                        let body = req
                            .body
                            .as_ref()
                            .map(|b| String::from_utf8_lossy(b).to_string())
                            .unwrap_or_default();
                        body.contains("\"in_reply_to\":\"del-req\"")
                            && body.contains("unknown operation: delete")
                            && body.contains("INTERNAL_ERROR")
                    });
                t.status(204);
            })
            .await;
        let client = open_client(&server.base_url());
        let req = Envelope {
            id: "del-req".into(),
            namespace: "ssh-agent".into(),
            msg_type: "request".into(),
            in_reply_to: String::new(),
            is_final: true,
            timestamp: "t".into(),
            payload: Some(serde_json::json!({"operation":"delete"})),
            shed: None,
        };
        handle_bus_message(
            &client,
            "ssh-agent",
            &req,
            &never_shutdown(),
            &test_handlers(approve_gate(), noop_audit(), empty_backend()),
        )
        .await;
        respond.assert_async().await;
    }

    #[tokio::test]
    async fn invalid_payload_internal_error() {
        // A payload that is not a JSON object (here a JSON array) is Go's
        // `json.Unmarshal(payload, &op)` failure → {invalid payload, INTERNAL_ERROR}.
        let server = MockServer::start_async().await;
        let respond = server
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/plugins/listeners/ssh-agent/respond")
                    .matches(|req| {
                        let body = req
                            .body
                            .as_ref()
                            .map(|b| String::from_utf8_lossy(b).to_string())
                            .unwrap_or_default();
                        body.contains("\"in_reply_to\":\"bad-req\"")
                            && body.contains("invalid payload")
                            && body.contains("INTERNAL_ERROR")
                    });
                t.status(204);
            })
            .await;
        let client = open_client(&server.base_url());
        let req = Envelope {
            id: "bad-req".into(),
            namespace: "ssh-agent".into(),
            msg_type: "request".into(),
            in_reply_to: String::new(),
            is_final: true,
            timestamp: "t".into(),
            payload: Some(serde_json::json!([1, 2, 3])),
            shed: None,
        };
        handle_bus_message(
            &client,
            "ssh-agent",
            &req,
            &never_shutdown(),
            &test_handlers(approve_gate(), noop_audit(), empty_backend()),
        )
        .await;
        respond.assert_async().await;
    }

    #[test]
    fn parse_operation_matches_go_unmarshal() {
        use serde_json::json;
        // Object with a string operation → that op.
        assert_eq!(parse_operation(&Some(json!({"operation":"list"}))), Ok("list".into()));
        // Object without operation, or operation null, or a bare null → "" (Go's zero).
        assert_eq!(parse_operation(&Some(json!({}))), Ok(String::new()));
        assert_eq!(parse_operation(&Some(json!({"operation":null}))), Ok(String::new()));
        assert_eq!(parse_operation(&Some(serde_json::Value::Null)), Ok(String::new()));
        // A non-object/non-null payload, or a non-string operation, is Go's unmarshal
        // error → invalid payload.
        assert_eq!(parse_operation(&Some(json!([1, 2]))), Err(()));
        assert_eq!(parse_operation(&Some(json!("hi"))), Err(()));
        assert_eq!(parse_operation(&Some(json!(123))), Err(()));
        assert_eq!(parse_operation(&Some(json!({"operation":123}))), Err(()));
    }

    // ---- list / status ops (mirror ssh_handler.go handleList/handleStatus) ----

    /// A backend with a configurable `list()` result + mode, for the list/status tests.
    struct ListStubBackend {
        list_result: Result<Vec<SshKeyInfo>, String>,
        mode: &'static str,
    }
    impl SshBackend for ListStubBackend {
        fn list(&self) -> Result<Vec<SshKeyInfo>, String> {
            self.list_result.clone()
        }
        fn sign(&self, _pk: &[u8], _d: &[u8], _f: u32) -> Result<SshSignature, String> {
            Err("key not found".to_string())
        }
        fn mode(&self) -> &str {
            self.mode
        }
    }

    #[test]
    fn ssh_list_responds_keys() {
        // Two keys → SSHListResponse{keys:[{format,blob(b64),comment}]} + an ok audit
        // via the positional Log form (detail="2 keys", approval="none", NO outcome).
        let backend: Arc<dyn SshBackend> = Arc::new(ListStubBackend {
            list_result: Ok(vec![
                SshKeyInfo {
                    format: "ssh-ed25519".into(),
                    blob: b"blob-ed".to_vec(),
                    comment: "id_ed25519".into(),
                },
                SshKeyInfo {
                    format: "ssh-rsa".into(),
                    blob: b"blob-rsa".to_vec(),
                    comment: "id_rsa".into(),
                },
            ]),
            mode: "local-keys",
        });
        let (payload, entry) = handle_list("", "web", &backend);
        assert_eq!(payload["keys"][0]["format"], "ssh-ed25519");
        assert_eq!(payload["keys"][0]["blob"], b64(b"blob-ed"));
        assert_eq!(payload["keys"][0]["comment"], "id_ed25519");
        assert_eq!(payload["keys"][1]["format"], "ssh-rsa");
        assert_eq!(payload["keys"][1]["comment"], "id_rsa");
        // Positional-Log audit shape: op=list, ok, detail="2 keys", approval=none,
        // NO decided_by/scope/ttl/code/reason.
        assert_eq!(entry.op, "list");
        assert_eq!(entry.ns, "ssh-agent");
        assert_eq!(entry.result, "ok");
        assert_eq!(entry.detail, "2 keys");
        assert_eq!(entry.approval, "none");
        assert_eq!(entry.shed, "web");
        assert_eq!(entry.server, "");
        assert_eq!(entry.decided_by, "");
        assert_eq!(entry.scope, "");
        assert_eq!(entry.ttl, "");
        assert_eq!(entry.code, "");
    }

    #[test]
    fn ssh_list_empty_serializes_as_empty_array() {
        // An empty backend serializes to {"keys":[]} (NOT null) — Go's make([]_,0).
        let backend: Arc<dyn SshBackend> = Arc::new(ListStubBackend {
            list_result: Ok(Vec::new()),
            mode: "local-keys",
        });
        let (payload, entry) = handle_list("", "", &backend);
        assert_eq!(payload, serde_json::json!({"keys": []}));
        assert_eq!(entry.detail, "0 keys");
    }

    #[test]
    fn ssh_list_error_returns_key_listing_failed() {
        // A backend list error → {key listing failed, INTERNAL_ERROR} + an error audit
        // whose detail carries the raw backend error string, approval=none.
        let backend: Arc<dyn SshBackend> = Arc::new(ListStubBackend {
            list_result: Err("agent: failed to list keys".into()),
            mode: "agent-forward",
        });
        let (payload, entry) = handle_list("", "web", &backend);
        assert_eq!(payload["error"], "key listing failed");
        assert_eq!(payload["code"], "INTERNAL_ERROR");
        assert_eq!(entry.result, "error");
        assert_eq!(entry.detail, "agent: failed to list keys");
        assert_eq!(entry.approval, "none");
        assert_eq!(entry.op, "list");
    }

    #[test]
    fn ssh_status_reports_mode_and_count() {
        // status → {connected:true, mode, key_count=list().len()}.
        let backend: Arc<dyn SshBackend> = Arc::new(ListStubBackend {
            list_result: Ok(vec![SshKeyInfo {
                format: "ssh-ed25519".into(),
                blob: b"b".to_vec(),
                comment: "c".into(),
            }]),
            mode: "local-keys",
        });
        assert_eq!(
            handle_status(&backend),
            serde_json::json!({"connected": true, "mode": "local-keys", "key_count": 1})
        );
        // A list error → key_count 0 (Go counts only when err==nil), mode still reported.
        let err_backend: Arc<dyn SshBackend> = Arc::new(ListStubBackend {
            list_result: Err("boom".into()),
            mode: "agent-forward",
        });
        assert_eq!(
            handle_status(&err_backend),
            serde_json::json!({"connected": true, "mode": "agent-forward", "key_count": 0})
        );
    }

    // ---- golden: SSH payload shapes (in-crate, following the load_discovered_servers
    //      controltoken.rs precedent — a bin crate has no lib target, so the golden
    //      that builds bus-internal serde types lives here, not in tests/golden.rs) ----

    #[test]
    fn golden_ssh_payload_shapes() {
        let path = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../tests/host-agent-diff/fixtures/ssh_payload_shapes.json");
        let data = std::fs::read_to_string(&path)
            .unwrap_or_else(|e| panic!("read {}: {e}", path.display()));
        let fx: serde_json::Value = serde_json::from_str(&data).unwrap();
        assert_eq!(
            fx["protocol_version"].as_i64(),
            Some(1),
            "ssh_payload_shapes.json protocol_version skew"
        );

        for v in fx["list_vectors"].as_array().expect("list_vectors") {
            let keys: Vec<SshKeyInfoResp> = v["input"]["keys"]
                .as_array()
                .expect("input.keys")
                .iter()
                .map(|k| SshKeyInfoResp {
                    format: k["format"].as_str().unwrap().to_string(),
                    blob: k["blob_b64"].as_str().unwrap().to_string(),
                    comment: k["comment"].as_str().unwrap().to_string(),
                })
                .collect();
            let got = serde_json::to_value(SshListResponse { keys }).unwrap();
            assert_eq!(got, v["expected"], "list vector {}", v["name"]);
        }

        for v in fx["sign_vectors"].as_array().expect("sign_vectors") {
            let got = serde_json::to_value(SshSignResponse {
                format: v["input"]["format"].as_str().unwrap().to_string(),
                blob: v["input"]["blob_b64"].as_str().unwrap().to_string(),
                rest: String::new(),
            })
            .unwrap();
            assert_eq!(got, v["expected"], "sign vector {}", v["name"]);
        }

        for v in fx["status_vectors"].as_array().expect("status_vectors") {
            let got = serde_json::to_value(SshStatusResponse {
                connected: true,
                mode: v["input"]["mode"].as_str().unwrap().to_string(),
                key_count: v["input"]["key_count"].as_u64().unwrap() as usize,
            })
            .unwrap();
            assert_eq!(got, v["expected"], "status vector {}", v["name"]);
        }

        for v in fx["error_vectors"].as_array().expect("error_vectors") {
            let got = ssh_error(
                v["input"]["error"].as_str().unwrap(),
                v["input"]["code"].as_str().unwrap(),
            );
            assert_eq!(got, v["expected"], "error vector {}", v["name"]);
        }
    }

    // ---- gated sign flow (mirrors ssh_handler.go:handleSign) ----

    #[tokio::test]
    async fn sign_approve_returns_signresponse_and_ok_audit() {
        let pub_bytes = fixed_ed25519_pub();
        let backend: Arc<dyn SshBackend> = Arc::new(StubBackend {
            pubkey: pub_bytes.clone(),
            result: Ok(SshSignature {
                format: "ssh-ed25519".to_string(),
                blob: vec![0xAB; 64],
            }),
        });
        let env = sign_env(&b64(&pub_bytes), &b64(b"challenge-bytes"), 0);
        let (payload, entry) = handle_sign(&env, "", "web", &approve_gate(), &backend).await;

        // SSHSignResponse{format, blob(b64 sig), rest:""}.
        assert_eq!(payload["format"], "ssh-ed25519");
        assert_eq!(payload["blob"], b64(&[0xAB; 64]));
        assert_eq!(payload["rest"], "");

        // ok audit: detail=key type, approval=approve-all, decided_by/scope/ttl empty.
        let entry = entry.expect("ok path audits");
        assert_eq!(entry.result, "ok");
        assert_eq!(entry.detail, "ssh-ed25519");
        assert_eq!(entry.ns, "ssh-agent");
        assert_eq!(entry.op, "sign");
        assert_eq!(entry.approval, "approve-all");
        assert_eq!(entry.server, ""); // single-server mode
        assert_eq!(entry.shed, "web");
        assert_eq!(entry.decided_by, "");
        assert_eq!(entry.code, "");
        assert_eq!(entry.reason, "");
    }

    #[tokio::test]
    async fn sign_deny_returns_error_and_denied_audit() {
        let env = sign_env(&b64(&fixed_ed25519_pub()), &b64(b"data"), 0);
        let (payload, entry) = handle_sign(&env, "", "web", &deny_gate(), &empty_backend()).await;
        // {approval denied, SIGN_FAILED}.
        assert_eq!(payload["error"], "approval denied");
        assert_eq!(payload["code"], "SIGN_FAILED");
        // denied audit: result=denied, approval=deny-all; NO detail, NO code, NO reason.
        let entry = entry.expect("deny path audits");
        assert_eq!(entry.result, "denied");
        assert_eq!(entry.approval, "deny-all");
        assert_eq!(entry.detail, "");
        assert_eq!(entry.code, "");
        assert_eq!(entry.reason, "");
        assert_eq!(entry.decided_by, ""); // deny-all's empty outcome
    }

    #[tokio::test]
    async fn sign_gate_runs_before_pubkey_decode() {
        // A DENY policy + a BAD public key must return "approval denied" (gate first),
        // NOT "invalid public key encoding" — this pins the ssh_handler.go ordering.
        let env = sign_env("!!!not-base64!!!", &b64(b"data"), 0);
        let (payload, entry) = handle_sign(&env, "", "web", &deny_gate(), &empty_backend()).await;
        assert_eq!(payload["error"], "approval denied");
        assert_eq!(payload["code"], "SIGN_FAILED");
        assert_eq!(entry.expect("deny audits").result, "denied");
    }

    #[tokio::test]
    async fn sign_bad_pubkey_b64_internal_error_no_audit() {
        let env = sign_env("!!!not-base64!!!", &b64(b"data"), 0);
        let (payload, entry) =
            handle_sign(&env, "", "web", &approve_gate(), &empty_backend()).await;
        assert_eq!(payload["error"], "invalid public key encoding");
        assert_eq!(payload["code"], "INTERNAL_ERROR");
        assert!(entry.is_none(), "parse-error paths do NOT audit");
    }

    #[tokio::test]
    async fn sign_unparsable_pubkey_key_not_found_no_audit() {
        // Valid base64, but not a valid SSH public key wire blob.
        let env = sign_env(&b64(b"definitely not an ssh key"), &b64(b"data"), 0);
        let (payload, entry) =
            handle_sign(&env, "", "web", &approve_gate(), &empty_backend()).await;
        assert_eq!(payload["error"], "invalid public key");
        assert_eq!(payload["code"], "KEY_NOT_FOUND");
        assert!(entry.is_none());
    }

    #[tokio::test]
    async fn sign_bad_data_b64_internal_error_no_audit() {
        let env = sign_env(&b64(&fixed_ed25519_pub()), "!!!bad!!!", 0);
        let (payload, entry) =
            handle_sign(&env, "", "web", &approve_gate(), &empty_backend()).await;
        assert_eq!(payload["error"], "invalid challenge data encoding");
        assert_eq!(payload["code"], "INTERNAL_ERROR");
        assert!(entry.is_none());
    }

    #[tokio::test]
    async fn sign_backend_error_sign_failed_and_error_audit() {
        // approve-all + a valid pubkey the (empty) backend doesn't hold -> key not
        // found -> {sign operation failed, SIGN_FAILED} + error audit (detail=key type).
        let env = sign_env(&b64(&fixed_ed25519_pub()), &b64(b"data"), 0);
        let (payload, entry) =
            handle_sign(&env, "", "web", &approve_gate(), &empty_backend()).await;
        assert_eq!(payload["error"], "sign operation failed");
        assert_eq!(payload["code"], "SIGN_FAILED");
        let entry = entry.expect("backend-error path audits");
        assert_eq!(entry.result, "error");
        assert_eq!(entry.detail, "ssh-ed25519");
        assert_eq!(entry.approval, "approve-all");
        assert_eq!(entry.code, ""); // ssh_handler.go's error audit sets no code
    }

    #[tokio::test]
    async fn sign_invalid_payload_internal_error_no_audit() {
        // `flags` as a string fails the SSHSignRequest decode -> {invalid sign
        // request, INTERNAL_ERROR}; the gate is never consulted, no audit.
        let env = Envelope {
            id: "sign-1".into(),
            namespace: "ssh-agent".into(),
            msg_type: "request".into(),
            in_reply_to: String::new(),
            is_final: true,
            timestamp: "t".into(),
            payload: Some(serde_json::json!({"operation":"sign","flags":"not-a-number"})),
            shed: None,
        };
        let (payload, entry) = handle_sign(&env, "", "", &approve_gate(), &empty_backend()).await;
        assert_eq!(payload["error"], "invalid sign request");
        assert_eq!(payload["code"], "INTERNAL_ERROR");
        assert!(entry.is_none());
    }

    #[tokio::test]
    async fn sign_ok_via_handle_bus_message_responds_and_audits() {
        // End-to-end through the shared dispatch: the response is POSTed AND the ok
        // entry reaches the audit sink.
        let server = MockServer::start_async().await;
        let respond = server
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/plugins/listeners/ssh-agent/respond")
                    .matches(|req| {
                        let body = req
                            .body
                            .as_ref()
                            .map(|b| String::from_utf8_lossy(b).to_string())
                            .unwrap_or_default();
                        body.contains("\"in_reply_to\":\"sign-1\"")
                            && body.contains("ssh-ed25519")
                            && body.contains("\"rest\":\"\"")
                    });
                t.status(204);
            })
            .await;
        let client = open_client(&server.base_url());
        let pub_bytes = fixed_ed25519_pub();
        let backend: Arc<dyn SshBackend> = Arc::new(StubBackend {
            pubkey: pub_bytes.clone(),
            result: Ok(SshSignature {
                format: "ssh-ed25519".to_string(),
                blob: vec![0x11; 64],
            }),
        });
        let audit_collector = Arc::new(CollectingAudit::default());
        let audit: Arc<dyn AuditSink> = audit_collector.clone();
        let gate = approve_gate();
        let env = sign_env(&b64(&pub_bytes), &b64(b"data"), 0);
        handle_bus_message(
            &client,
            "ssh-agent",
            &env,
            &never_shutdown(),
            &test_handlers(gate, audit, backend),
        )
        .await;
        respond.assert_async().await;
        let entries = audit_collector.entries.lock().unwrap();
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].result, "ok");
        assert_eq!(entries[0].shed, "web"); // echoed from the request's shed
    }

    #[tokio::test]
    async fn static_token_provider_open_mode_sends_nothing() {
        // A StaticTokenProvider("") behaves as open mode: bearer() → None.
        let client = BusClient::new(
            "http://x".to_string(),
            String::new(),
            Some(Arc::new(StaticTokenProvider::new(""))),
            None,
            silent_log(),
        )
        .unwrap();
        assert_eq!(client.bearer().await, None);
        // A non-empty static provider sends its token.
        let client = BusClient::new(
            "http://x".to_string(),
            String::new(),
            Some(Arc::new(StaticTokenProvider::new("shed_creds_abc"))),
            None,
            silent_log(),
        )
        .unwrap();
        assert_eq!(client.bearer().await, Some("shed_creds_abc".to_string()));
    }

    #[test]
    fn bus_namespaces_are_the_three_credential_namespaces() {
        assert_eq!(
            BUS_NAMESPACES,
            ["ssh-agent", "aws-credentials", "docker-credentials"]
        );
    }

    // ---- AWS credential handler (mirror aws_handler.go) --------------------------

    use crate::aws_backend::CachedCreds;

    /// A scripted AWS backend: `get_credentials` returns a canned Result (and counts
    /// calls, to prove the gate short-circuits a deny); `status` returns a canned
    /// `(role, expiry)`.
    struct FakeAwsBackend {
        creds: Result<CachedCreds, String>,
        status: (String, Option<i64>),
        calls: AtomicUsize,
    }
    impl FakeAwsBackend {
        fn new(creds: Result<CachedCreds, String>, status: (String, Option<i64>)) -> Arc<Self> {
            Arc::new(Self {
                creds,
                status,
                calls: AtomicUsize::new(0),
            })
        }
        fn creds_ok(creds: CachedCreds) -> Arc<Self> {
            Self::new(Ok(creds), ("unused".into(), None))
        }
        fn creds_err(msg: &str) -> Arc<Self> {
            Self::new(Err(msg.to_string()), ("unused".into(), None))
        }
        fn with_status(role: &str, until: Option<i64>) -> Arc<Self> {
            Self::new(Err("unused".into()), (role.to_string(), until))
        }
    }
    #[async_trait::async_trait]
    impl AwsBackend for FakeAwsBackend {
        async fn get_credentials(&self, _server: &str, _shed: &str) -> Result<CachedCreds, String> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            self.creds.clone()
        }
        fn status(&self, _server: &str, _shed: &str) -> (String, Option<i64>) {
            self.status.clone()
        }
    }

    fn creds(exp: Option<i64>) -> CachedCreds {
        CachedCreds {
            access_key_id: "ASIATESTKEY".into(),
            secret_access_key: "SECRETKEY".into(),
            session_token: "SESSIONTOKEN".into(),
            expiration: exp,
        }
    }

    fn aws_handlers(backend: Arc<dyn AwsBackend>, gate: Arc<dyn ApprovalGate>) -> AwsHandlers {
        AwsHandlers { backend, gate }
    }

    /// Build an aws-credentials request Envelope carrying `payload` + a fixed shed.
    fn aws_env(payload: serde_json::Value) -> Envelope {
        Envelope {
            id: "aws-req".into(),
            namespace: "aws-credentials".into(),
            msg_type: "request".into(),
            in_reply_to: String::new(),
            is_final: true,
            timestamp: "t".into(),
            payload: Some(payload),
            shed: Some(ShedInfo {
                name: "web".into(),
                backend: "vz".into(),
                server: "mini2".into(),
            }),
        }
    }

    #[tokio::test]
    async fn aws_get_credentials_ok() {
        // 4071049445 == 2099-01-02T15:04:05Z.
        let backend: Arc<dyn AwsBackend> = FakeAwsBackend::creds_ok(creds(Some(4071049445)));
        // Single-server mode: server_name is empty (matches Go's h.server).
        let (payload, entry) =
            handle_aws_get_credentials("", "web", &approve_gate(), &backend).await;
        assert_eq!(payload["access_key_id"], "ASIATESTKEY");
        assert_eq!(payload["secret_access_key"], "SECRETKEY");
        assert_eq!(payload["session_token"], "SESSIONTOKEN");
        assert_eq!(payload["expiration"], "2099-01-02T15:04:05Z");
        // ok audit: ns/op fixed, detail = awsExpiryDetail, approval, empty outcome.
        let entry = entry.expect("ok path audits");
        assert_eq!(entry.ns, "aws-credentials");
        assert_eq!(entry.op, "get_credentials");
        assert_eq!(entry.result, "ok");
        assert_eq!(entry.detail, "expires:15:04");
        assert_eq!(entry.approval, "approve-all");
        assert_eq!(entry.shed, "web");
        assert_eq!(entry.server, "");
        // aws_handler.go sets NO code/reason on ANY get_credentials audit.
        assert_eq!(entry.code, "");
        assert_eq!(entry.reason, "");
    }

    #[tokio::test]
    async fn aws_expiration_omitted_when_zero() {
        // No expiry hint → the expiration key is ABSENT (Go omitempty), audit expires:none.
        let backend: Arc<dyn AwsBackend> = FakeAwsBackend::creds_ok(creds(None));
        let (payload, entry) =
            handle_aws_get_credentials("", "web", &approve_gate(), &backend).await;
        assert!(
            payload.get("expiration").is_none(),
            "expiration key must be absent for a None expiry, got {payload}"
        );
        assert_eq!(payload["access_key_id"], "ASIATESTKEY");
        assert_eq!(entry.expect("ok path audits").detail, "expires:none");
    }

    #[test]
    fn aws_expiry_detail_format() {
        // The handler's audit detail helper (aws_backend::aws_expiry_detail): none / HH:MM.
        assert_eq!(aws_expiry_detail(None), "expires:none");
        assert_eq!(aws_expiry_detail(Some(4071049445)), "expires:15:04");
    }

    #[tokio::test]
    async fn aws_ping() {
        // ping → {"status":"ok"}, no audit.
        let aws = aws_handlers(FakeAwsBackend::creds_err("unused"), approve_gate());
        let (payload, entry) = aws_dispatch(&aws_env(serde_json::json!({"operation":"ping"})), &aws, "").await;
        assert_eq!(payload, serde_json::json!({"status": "ok"}));
        assert!(entry.is_none(), "ping does not audit");
    }

    #[tokio::test]
    async fn aws_backend_error_maps_assume_role_failed() {
        let backend: Arc<dyn AwsBackend> =
            FakeAwsBackend::creds_err("sts:AssumeRole failed for arn:aws:iam::9:role/x: boom");
        let (payload, entry) =
            handle_aws_get_credentials("srv", "web", &approve_gate(), &backend).await;
        // The guest gets the generic mapping; ASSUME_ROLE_FAILED is get_credentials-scoped.
        assert_eq!(payload["error"], "credential request failed");
        assert_eq!(payload["code"], "ASSUME_ROLE_FAILED");
        // error audit: result=error, detail=raw backend error, approval; NO code.
        let entry = entry.expect("error path audits");
        assert_eq!(entry.result, "error");
        assert_eq!(
            entry.detail,
            "sts:AssumeRole failed for arn:aws:iam::9:role/x: boom"
        );
        assert_eq!(entry.approval, "approve-all");
        assert_eq!(entry.code, "");
        assert_eq!(entry.reason, "");
    }

    #[tokio::test]
    async fn aws_status_role_and_cached_until() {
        // Some(expiry) → cached_until rendered literal-Z; status never audits.
        let aws = aws_handlers(
            FakeAwsBackend::with_status("passthrough:shed-test", Some(4071049445)),
            approve_gate(),
        );
        let (payload, entry) =
            aws_dispatch(&aws_env(serde_json::json!({"operation":"status"})), &aws, "").await;
        assert_eq!(
            payload,
            serde_json::json!({"connected": true, "role": "passthrough:shed-test", "cached_until": "2099-01-02T15:04:05Z"})
        );
        assert!(entry.is_none(), "status does not audit");
        // None → cached_until omitted (Go covers only the nil case; Rust covers both).
        let aws = aws_handlers(
            FakeAwsBackend::with_status("arn:aws:iam::9:role/x", None),
            approve_gate(),
        );
        let (payload, _) =
            aws_dispatch(&aws_env(serde_json::json!({"operation":"status"})), &aws, "").await;
        assert_eq!(
            payload,
            serde_json::json!({"connected": true, "role": "arn:aws:iam::9:role/x"})
        );
        assert!(payload.get("cached_until").is_none());
    }

    #[tokio::test]
    async fn aws_denied_by_gate() {
        // A deny-all gate rejects before the backend is hit; the denied audit carries NO
        // detail/code/reason and the empty deny-all outcome.
        let backend = FakeAwsBackend::creds_ok(creds(Some(4071049445)));
        let dyn_backend: Arc<dyn AwsBackend> = backend.clone();
        let (payload, entry) =
            handle_aws_get_credentials("srv", "web", &deny_gate(), &dyn_backend).await;
        assert_eq!(payload["error"], "approval denied");
        assert_eq!(payload["code"], "ASSUME_ROLE_FAILED");
        let entry = entry.expect("deny path audits");
        assert_eq!(entry.result, "denied");
        assert_eq!(entry.approval, "deny-all");
        assert_eq!(entry.detail, "");
        assert_eq!(entry.code, "");
        assert_eq!(entry.reason, "");
        assert_eq!(entry.decided_by, ""); // deny-all's empty outcome
        assert_eq!(
            backend.calls.load(Ordering::SeqCst),
            0,
            "backend must not be consulted when the gate denies"
        );
    }

    #[tokio::test]
    async fn aws_unknown_op_exact_string() {
        let aws = aws_handlers(FakeAwsBackend::creds_err("unused"), approve_gate());
        let (payload, entry) =
            aws_dispatch(&aws_env(serde_json::json!({"operation": "delete"})), &aws, "").await;
        assert_eq!(payload["error"], "unknown operation: delete");
        assert_eq!(payload["code"], "INTERNAL_ERROR");
        assert!(entry.is_none());
    }

    #[tokio::test]
    async fn aws_invalid_payload_internal_error() {
        // A non-object payload → {invalid payload, INTERNAL_ERROR} (AWS code, shared parse).
        let aws = aws_handlers(FakeAwsBackend::creds_err("unused"), approve_gate());
        let (payload, entry) = aws_dispatch(&aws_env(serde_json::json!([1, 2, 3])), &aws, "").await;
        assert_eq!(payload["error"], "invalid payload");
        assert_eq!(payload["code"], "INTERNAL_ERROR");
        assert!(entry.is_none());
    }

    #[tokio::test]
    async fn aws_uses_aws_gate_not_ssh() {
        // One BusHandlers: ssh gate = deny-all, aws gate = approve-all. Routed through the
        // real dispatcher ([`dispatch_bus_message`]) + the wire, an aws get_credentials is
        // APPROVED (uses aws.gate → the mock only matches a vended-key body) while an ssh
        // sign is DENIED (uses the ssh gate → the mock only matches an "approval denied"
        // body). Each mock's `.assert()` fails if the wrong gate was consulted, proving
        // per-namespace gate selection (F6).
        let handlers = BusHandlers {
            gate: deny_gate(),                                  // ssh gate: deny-all
            audit: noop_audit(),
            backend: empty_backend(),
            server_name: String::new(),
            aws: Some(AwsHandlers {
                backend: FakeAwsBackend::creds_ok(creds(None)), // aws gate: approve-all
                gate: approve_gate(),
            }),
            docker: None,
        };

        // aws-credentials → the approve-all aws gate vends the key (never "approval denied").
        let server = MockServer::start_async().await;
        let aws_ok = server
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/plugins/listeners/aws-credentials/respond")
                    .matches(|req| {
                        let body = req
                            .body
                            .as_ref()
                            .map(|b| String::from_utf8_lossy(b).to_string())
                            .unwrap_or_default();
                        body.contains("ASIATESTKEY") && !body.contains("approval denied")
                    });
                t.status(204);
            })
            .await;
        let client = open_client(&server.base_url());
        dispatch_bus_message(
            &client,
            "aws-credentials",
            &aws_env(serde_json::json!({"operation": "get_credentials"})),
            &never_shutdown(),
            &handlers,
        )
        .await;
        aws_ok.assert_async().await;

        // ssh-agent → the deny-all ssh gate rejects the sign (never a signature response).
        let ssh_server = MockServer::start_async().await;
        let ssh_denied = ssh_server
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/plugins/listeners/ssh-agent/respond")
                    .matches(|req| {
                        let body = req
                            .body
                            .as_ref()
                            .map(|b| String::from_utf8_lossy(b).to_string())
                            .unwrap_or_default();
                        body.contains("approval denied") && body.contains("SIGN_FAILED")
                    });
                t.status(204);
            })
            .await;
        let ssh_client = open_client(&ssh_server.base_url());
        dispatch_bus_message(
            &ssh_client,
            "ssh-agent",
            &sign_env(&b64(&fixed_ed25519_pub()), &b64(b"data"), 0),
            &never_shutdown(),
            &handlers,
        )
        .await;
        ssh_denied.assert_async().await;
    }

    #[test]
    fn aws_payload_tag_names_match_protocol() {
        // Golden-consistency: the serde tag names equal the Go json tags in aws.go.
        assert_eq!(
            serde_json::to_value(AwsCredentialsResponse {
                access_key_id: "a".into(),
                secret_access_key: "b".into(),
                session_token: "c".into(),
                expiration: Some("2099-01-02T15:04:05Z".into()),
            })
            .unwrap(),
            serde_json::json!({"access_key_id":"a","secret_access_key":"b","session_token":"c","expiration":"2099-01-02T15:04:05Z"})
        );
        assert_eq!(
            serde_json::to_value(AwsCredentialsResponse {
                access_key_id: "a".into(),
                secret_access_key: "b".into(),
                session_token: "c".into(),
                expiration: None,
            })
            .unwrap(),
            serde_json::json!({"access_key_id":"a","secret_access_key":"b","session_token":"c"})
        );
        assert_eq!(
            serde_json::to_value(AwsStatusResponse {
                connected: true,
                role: "passthrough:p".into(),
                cached_until: Some("2099-01-02T15:04:05Z".into()),
            })
            .unwrap(),
            serde_json::json!({"connected":true,"role":"passthrough:p","cached_until":"2099-01-02T15:04:05Z"})
        );
        assert_eq!(
            serde_json::to_value(AwsStatusResponse {
                connected: true,
                role: "passthrough:p".into(),
                cached_until: None,
            })
            .unwrap(),
            serde_json::json!({"connected":true,"role":"passthrough:p"})
        );
        assert_eq!(
            serde_json::to_value(AwsPingResponse {
                status: "ok".into()
            })
            .unwrap(),
            serde_json::json!({"status":"ok"})
        );
        assert_eq!(
            aws_error("boom", "INTERNAL_ERROR"),
            serde_json::json!({"error":"boom","code":"INTERNAL_ERROR"})
        );
    }

    // ---- Docker credential handler (mirror docker_handler.go) --------------------

    use crate::docker_backend::{DockerCredError, DockerCredential};

    /// A scripted Docker backend: `get_credentials` returns a canned Result (and counts
    /// calls, to prove the gate short-circuits a deny); `list_credentials`/`status`
    /// return canned values.
    struct FakeDockerBackend {
        get: Result<DockerCredential, DockerCredError>,
        list: Result<BTreeMap<String, String>, String>,
        status: (bool, usize),
        get_calls: AtomicUsize,
    }
    impl FakeDockerBackend {
        fn new(
            get: Result<DockerCredential, DockerCredError>,
            list: Result<BTreeMap<String, String>, String>,
            status: (bool, usize),
        ) -> Arc<Self> {
            Arc::new(Self {
                get,
                list,
                status,
                get_calls: AtomicUsize::new(0),
            })
        }
        fn get_ok(cred: DockerCredential) -> Arc<Self> {
            Self::new(Ok(cred), Ok(BTreeMap::new()), (false, 0))
        }
        fn get_err(e: DockerCredError) -> Arc<Self> {
            Self::new(Err(e), Ok(BTreeMap::new()), (false, 0))
        }
        fn with_list(list: BTreeMap<String, String>) -> Arc<Self> {
            Self::new(Err(unused_err()), Ok(list), (false, 0))
        }
        fn list_err() -> Arc<Self> {
            Self::new(Err(unused_err()), Err("boom".to_string()), (false, 0))
        }
        fn with_status(allow_all: bool, count: usize) -> Arc<Self> {
            Self::new(Err(unused_err()), Ok(BTreeMap::new()), (allow_all, count))
        }
    }
    #[async_trait::async_trait]
    impl DockerBackend for FakeDockerBackend {
        async fn get_credentials(
            &self,
            _server: &str,
            _shed: &str,
            _server_url: &str,
        ) -> Result<DockerCredential, DockerCredError> {
            self.get_calls.fetch_add(1, Ordering::SeqCst);
            self.get.clone()
        }
        async fn list_credentials(
            &self,
            _server: &str,
            _shed: &str,
        ) -> Result<BTreeMap<String, String>, String> {
            self.list.clone()
        }
        fn status(&self, _server: &str, _shed: &str) -> (bool, usize) {
            self.status
        }
    }

    fn unused_err() -> DockerCredError {
        DockerCredError {
            msg: "unused".to_string(),
            code: "UNUSED".to_string(),
        }
    }
    fn cred(server_url: &str, username: &str, secret: &str) -> DockerCredential {
        DockerCredential {
            server_url: server_url.to_string(),
            username: username.to_string(),
            secret: secret.to_string(),
        }
    }
    fn docker_handlers(backend: Arc<dyn DockerBackend>, gate: Arc<dyn ApprovalGate>) -> DockerHandlers {
        DockerHandlers { backend, gate }
    }
    /// A docker-credentials request Envelope carrying `payload` + a fixed shed.
    fn docker_env(payload: serde_json::Value) -> Envelope {
        Envelope {
            id: "docker-req".into(),
            namespace: "docker-credentials".into(),
            msg_type: "request".into(),
            in_reply_to: String::new(),
            is_final: true,
            timestamp: "t".into(),
            payload: Some(payload),
            shed: Some(ShedInfo {
                name: "web".into(),
                backend: "vz".into(),
                server: "mini2".into(),
            }),
        }
    }
    fn docker_get_env(server_url: &str) -> Envelope {
        docker_env(serde_json::json!({"operation": "get", "server_url": server_url}))
    }

    #[tokio::test]
    async fn docker_get_ok() {
        let backend: Arc<dyn DockerBackend> = FakeDockerBackend::get_ok(cred("ghcr.io", "u", "s"));
        let (payload, entry) =
            docker_dispatch(&docker_get_env("ghcr.io"), &docker_handlers(backend, approve_gate()), "").await;
        assert_eq!(payload["server_url"], "ghcr.io");
        assert_eq!(payload["username"], "u");
        assert_eq!(payload["secret"], "s");
        // ok audit: ns/op fixed, detail=server_url, approval; NO code/reason.
        let entry = entry.expect("ok path audits");
        assert_eq!(entry.ns, "docker-credentials");
        assert_eq!(entry.op, "get");
        assert_eq!(entry.result, "ok");
        assert_eq!(entry.detail, "ghcr.io");
        assert_eq!(entry.approval, "approve-all");
        assert_eq!(entry.shed, "web");
        assert_eq!(entry.code, "");
        assert_eq!(entry.reason, "");
    }

    #[tokio::test]
    async fn docker_get_error_maps_code() {
        // The guest receives the backend's raw code (REGISTRY_NOT_ALLOWED here).
        let backend: Arc<dyn DockerBackend> = FakeDockerBackend::get_err(DockerCredError {
            msg: "registry not allowed".into(),
            code: DOCKER_CODE_NOT_ALLOWED.into(),
        });
        let (payload, _entry) =
            docker_dispatch(&docker_get_env("blocked.io"), &docker_handlers(backend, approve_gate()), "").await;
        assert_eq!(payload["error"], "credential request failed");
        assert_eq!(payload["code"], "REGISTRY_NOT_ALLOWED");
    }

    async fn docker_get_audit_case(code: &str, msg: &str) -> AuditEntry {
        let backend: Arc<dyn DockerBackend> = FakeDockerBackend::get_err(DockerCredError {
            msg: msg.into(),
            code: code.into(),
        });
        let (_p, entry) = docker_dispatch(
            &docker_get_env("https://index.docker.io/v1/"),
            &docker_handlers(backend, approve_gate()),
            "",
        )
        .await;
        entry.expect("get error path audits")
    }

    #[tokio::test]
    async fn docker_get_audit_anonymous_on_not_found() {
        // CREDENTIALS_NOT_FOUND for an allowed registry → audit result "anonymous"
        // (guest pulls anonymously), code+reason+detail set.
        let e = docker_get_audit_case(DOCKER_CODE_NOT_FOUND, "no credentials found").await;
        assert_eq!(e.result, "anonymous");
        assert_eq!(e.code, "CREDENTIALS_NOT_FOUND");
        assert_eq!(e.reason, "no credentials found");
        assert_eq!(e.detail, "https://index.docker.io/v1/");
        assert_eq!(e.op, "get");
    }

    #[tokio::test]
    async fn docker_get_audit_error_on_not_allowed() {
        // An allowlist deny (a backend REGISTRY_NOT_ALLOWED) stays "error", not anonymous.
        let e = docker_get_audit_case(DOCKER_CODE_NOT_ALLOWED, "registry not allowed").await;
        assert_eq!(e.result, "error");
        assert_eq!(e.code, "REGISTRY_NOT_ALLOWED");
        assert_eq!(e.reason, "registry not allowed");
    }

    #[tokio::test]
    async fn docker_get_audit_error_on_helper_failed() {
        let e = docker_get_audit_case("HELPER_FAILED", "boom").await;
        assert_eq!(e.result, "error");
        assert_eq!(e.code, "HELPER_FAILED");
        assert_eq!(e.reason, "boom");
    }

    #[tokio::test]
    async fn docker_get_denied_guest_not_allowed_audit_approval_denied() {
        // A deny-all gate: the guest still receives REGISTRY_NOT_ALLOWED (back-compat)
        // while the audit carries APPROVAL_DENIED + result denied (two-code
        // disambiguation) + the deny reason. The backend is never consulted.
        let backend = FakeDockerBackend::get_ok(cred("ghcr.io", "u", "s"));
        let dyn_backend: Arc<dyn DockerBackend> = backend.clone();
        let (payload, entry) =
            docker_dispatch(&docker_get_env("ghcr.io"), &docker_handlers(dyn_backend, deny_gate()), "").await;
        assert_eq!(payload["error"], "approval denied");
        assert_eq!(payload["code"], "REGISTRY_NOT_ALLOWED");
        let entry = entry.expect("deny path audits");
        assert_eq!(entry.result, "denied");
        assert_eq!(entry.code, "APPROVAL_DENIED");
        assert_eq!(entry.reason, "denied: approval policy is deny-all");
        assert_eq!(entry.detail, "ghcr.io");
        assert_eq!(entry.approval, "deny-all");
        assert_eq!(entry.decided_by, ""); // deny-all's empty outcome
        assert_eq!(
            backend.get_calls.load(Ordering::SeqCst),
            0,
            "backend must not be consulted when the gate denies"
        );
    }

    #[tokio::test]
    async fn docker_ping() {
        let backend: Arc<dyn DockerBackend> = FakeDockerBackend::get_ok(cred("x", "y", "z"));
        let (payload, entry) =
            docker_dispatch(&docker_env(serde_json::json!({"operation":"ping"})), &docker_handlers(backend, approve_gate()), "").await;
        assert_eq!(payload, serde_json::json!({"status": "ok"}));
        assert!(entry.is_none(), "ping does not audit");
    }

    #[tokio::test]
    async fn docker_status_connected() {
        let backend: Arc<dyn DockerBackend> = FakeDockerBackend::with_status(true, 3);
        let (payload, entry) =
            docker_dispatch(&docker_env(serde_json::json!({"operation":"status"})), &docker_handlers(backend, approve_gate()), "").await;
        assert_eq!(
            payload,
            serde_json::json!({"connected": true, "allow_all": true, "registry_count": 3})
        );
        assert!(entry.is_none(), "status does not audit");
    }

    #[tokio::test]
    async fn docker_list_ok() {
        let mut m = BTreeMap::new();
        m.insert("gcr.io".to_string(), "user1".to_string());
        m.insert("ghcr.io".to_string(), "user2".to_string());
        let backend: Arc<dyn DockerBackend> = FakeDockerBackend::with_list(m);
        let (payload, entry) =
            docker_dispatch(&docker_env(serde_json::json!({"operation":"list"})), &docker_handlers(backend, approve_gate()), "").await;
        assert_eq!(payload["registries"]["gcr.io"], "user1");
        assert_eq!(payload["registries"]["ghcr.io"], "user2");
        // Positional audit: op=list, ok, detail="count:2", approval=none, NO outcome/code.
        let entry = entry.expect("list success audits");
        assert_eq!(entry.op, "list");
        assert_eq!(entry.ns, "docker-credentials");
        assert_eq!(entry.result, "ok");
        assert_eq!(entry.detail, "count:2");
        assert_eq!(entry.approval, "none");
        assert_eq!(entry.decided_by, "");
        assert_eq!(entry.code, "");
    }

    #[tokio::test]
    async fn docker_list_error_has_no_audit() {
        // A backend list error → {list failed, INTERNAL_ERROR} + NO audit (Go returns
        // early; list audits ONLY on success).
        let backend: Arc<dyn DockerBackend> = FakeDockerBackend::list_err();
        let (payload, entry) =
            docker_dispatch(&docker_env(serde_json::json!({"operation":"list"})), &docker_handlers(backend, approve_gate()), "").await;
        assert_eq!(payload["error"], "list failed");
        assert_eq!(payload["code"], "INTERNAL_ERROR");
        assert!(entry.is_none(), "list error path is audit-silent");
    }

    fn dummy_docker() -> Arc<dyn DockerBackend> {
        FakeDockerBackend::get_ok(cred("x", "y", "z"))
    }

    #[tokio::test]
    async fn docker_omitted_payload_invalid() {
        // An OMITTED payload (Go nil json.RawMessage) → {invalid payload, INTERNAL_ERROR}.
        let mut env = docker_env(serde_json::json!({"operation":"ping"}));
        env.payload = None;
        let (payload, entry) =
            docker_dispatch(&env, &docker_handlers(dummy_docker(), approve_gate()), "").await;
        assert_eq!(payload["error"], "invalid payload");
        assert_eq!(payload["code"], "INTERNAL_ERROR");
        assert!(entry.is_none());
    }

    #[tokio::test]
    async fn docker_null_payload_unknown_op_empty() {
        // Explicit null payload → zero op → "unknown operation: " (INTERNAL_ERROR).
        let mut env = docker_env(serde_json::json!({"operation":"ping"}));
        env.payload = Some(serde_json::Value::Null);
        let (payload, entry) =
            docker_dispatch(&env, &docker_handlers(dummy_docker(), approve_gate()), "").await;
        assert_eq!(payload["error"], "unknown operation: ");
        assert_eq!(payload["code"], "INTERNAL_ERROR");
        assert!(entry.is_none());
    }

    #[tokio::test]
    async fn docker_missing_op_unknown_op_empty() {
        // Object with no `operation` → zero op → "unknown operation: ".
        let (payload, entry) =
            docker_dispatch(&docker_env(serde_json::json!({})), &docker_handlers(dummy_docker(), approve_gate()), "").await;
        assert_eq!(payload["error"], "unknown operation: ");
        assert_eq!(payload["code"], "INTERNAL_ERROR");
        assert!(entry.is_none());
    }

    #[tokio::test]
    async fn docker_unknown_op_exact_string() {
        let (payload, entry) = docker_dispatch(
            &docker_env(serde_json::json!({"operation":"delete"})),
            &docker_handlers(dummy_docker(), approve_gate()),
            "",
        )
        .await;
        assert_eq!(payload["error"], "unknown operation: delete");
        assert_eq!(payload["code"], "INTERNAL_ERROR");
        assert!(entry.is_none());
    }

    #[tokio::test]
    async fn docker_array_or_scalar_op_invalid() {
        // A non-object payload (array or scalar) OR a non-string `operation` → the shared
        // parse_operation error → {invalid payload, INTERNAL_ERROR}.
        for payload in [
            serde_json::json!([1, 2, 3]),
            serde_json::json!("hi"),
            serde_json::json!({"operation": 123}),
        ] {
            let (resp, entry) =
                docker_dispatch(&docker_env(payload), &docker_handlers(dummy_docker(), approve_gate()), "").await;
            assert_eq!(resp["error"], "invalid payload");
            assert_eq!(resp["code"], "INTERNAL_ERROR");
            assert!(entry.is_none());
        }
    }

    #[tokio::test]
    async fn docker_uses_docker_gate_not_ssh_or_aws() {
        // One BusHandlers: ssh gate = deny-all, docker gate = approve-all. Routed through
        // the real dispatcher over the wire, a docker `get` is APPROVED (uses docker.gate,
        // never the ssh gate) → the mock only matches a vended-credential body. Proves
        // per-namespace gate selection.
        let docker_backend: Arc<dyn DockerBackend> = FakeDockerBackend::get_ok(cred("ghcr.io", "u", "s"));
        let handlers = BusHandlers {
            gate: deny_gate(), // ssh gate: deny-all
            audit: noop_audit(),
            backend: empty_backend(),
            server_name: String::new(),
            aws: None,
            docker: Some(DockerHandlers {
                backend: docker_backend,
                gate: approve_gate(), // docker gate: approve-all
            }),
        };
        let server = MockServer::start_async().await;
        let ok = server
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/plugins/listeners/docker-credentials/respond")
                    .matches(|req| {
                        let body = req
                            .body
                            .as_ref()
                            .map(|b| String::from_utf8_lossy(b).to_string())
                            .unwrap_or_default();
                        body.contains("\"secret\":\"s\"") && !body.contains("approval denied")
                    });
                t.status(204);
            })
            .await;
        let client = open_client(&server.base_url());
        dispatch_bus_message(
            &client,
            "docker-credentials",
            &docker_get_env("ghcr.io"),
            &never_shutdown(),
            &handlers,
        )
        .await;
        ok.assert_async().await;
    }

    #[test]
    fn docker_payload_tag_names_match_protocol() {
        // Golden-consistency: the guest-wire serde tag names equal the Go json tags in
        // docker.go (distinct from the Capitalized helper-protocol struct in
        // docker_backend.rs, whose roundtrip is guarded there).
        assert_eq!(
            serde_json::to_value(DockerGetResponse {
                server_url: "a".into(),
                username: "b".into(),
                secret: "c".into(),
            })
            .unwrap(),
            serde_json::json!({"server_url":"a","username":"b","secret":"c"})
        );
        let mut m = BTreeMap::new();
        m.insert("ghcr.io".to_string(), "u".to_string());
        assert_eq!(
            serde_json::to_value(DockerListResponse { registries: m }).unwrap(),
            serde_json::json!({"registries":{"ghcr.io":"u"}})
        );
        assert_eq!(
            serde_json::to_value(DockerPingResponse {
                status: "ok".into()
            })
            .unwrap(),
            serde_json::json!({"status":"ok"})
        );
        assert_eq!(
            serde_json::to_value(DockerStatusResponse {
                connected: true,
                allow_all: false,
                registry_count: 2,
            })
            .unwrap(),
            serde_json::json!({"connected":true,"allow_all":false,"registry_count":2})
        );
        assert_eq!(
            docker_error("boom", "INTERNAL_ERROR"),
            serde_json::json!({"error":"boom","code":"INTERNAL_ERROR"})
        );
    }
}
