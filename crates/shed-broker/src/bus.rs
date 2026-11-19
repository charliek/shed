//! Surface B — the shed-server plugin message bus (Rust port of `sdk/hostclient.go`
//! + `sdk/envelope.go`).
//!
//! [`BusClient`] is the host-agent's `HostClient` equivalent: it subscribes to a
//! namespace's SSE stream, receives request [`Envelope`]s, and `respond`s with
//! response envelopes routed back to the originating shed. It reuses shed-core's
//! transport primitives — the SSE framer (`shed_core::sse::SseParser`) and the
//! leaf-cert TLS pin verifier (`shed_core::tls::pinned_client_config`) — over a
//! reqwest+rustls client, so the wire behavior matches the Go clients byte
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
//! The machine pin (plan 035 §3.4), on top of that:
//!   * every subscribe and every respond carries `X-Shed-Machine-Id` (the 32-hex id) and
//!     `X-Shed-Machine-Name` (the display name, percent-encoded UTF-8) when this machine
//!     has an identity ([`crate::machine`]); with none, neither header is sent and the
//!     wire is exactly as before. An older server ignores both;
//!   * subscribe answers are classified by [`classify_subscribe_refusal`]:
//!
//!     | status | `error.code` | state | what the loop does |
//!     |---|---|---|---|
//!     | 403 | `SOURCE_NOT_BOUND` | `not-source` (names the active machine) | backoff and retry, no re-mint |
//!     | 409 | `SOURCE_DUPLICATE_STREAM` | `duplicate` | backoff and retry, no re-mint |
//!     | 409 | anything else, or no parseable body | `rejected` | stop (terminal, as before) |
//!     | 401 | any | `reconnecting` | re-mint, then backoff and retry (as before) |
//!     | any other, a 403 with another code or an unparseable body included | any | `reconnecting`, the server's body shown | backoff and retry (as before) |
//!
//!   * a refusal body is read within a byte AND a time bound; one that was cut short
//!     (over 1024 bytes, too slow, a read error) is never classified as a pin refusal:
//!     a 403 is then plain `reconnecting`, a 409 terminal;
//!   * a `429 SOURCE_RATE_LIMITED` (too many new or changed machines for this user) is
//!     retried no sooner than its whole-seconds `Retry-After` (capped at a minute), and
//!     never sooner than the current backoff, and a `retry_now` does not cut that wait
//!     short (it cuts only the ordinary backoff);
//!   * a client built with a presence channel ([`BusClient::with_presence`]: the daemon,
//!     never the desktop's embedded broker) stands by while the desktop app's embedded
//!     broker runs on this machine ([`crate::presence`]: a lock the app holds, which the
//!     daemon's one watcher looks at every [`PRESENCE_CHECK_INTERVAL`] and publishes).
//!     While the channel says present the client neither subscribes nor answers: it
//!     subscribes nothing (state `yielding`), drops a connect or a held stream at once,
//!     leaves an ordinary backoff sleep at once (a `429`'s wait is still waited out),
//!     starts no handler for a request it has received, and posts no answer (checked
//!     immediately before every send; audited `undelivered`, left for the desktop app).
//!     Such requests stay pending on the server, which re-delivers them to the app. Once
//!     it says absent the client subscribes at once, from the backoff floor. A
//!     `409 SOURCE_DUPLICATE_STREAM` is retried on the backoff, as always: with the lock
//!     it is left for the time before the daemon's next look, and for two daemons on a
//!     copied state directory;
//!   * a client holding the presence lock ([`BusClient::with_presence_lock`]: the
//!     embedded broker only) retries an older server's terminal `409` for
//!     [`OLD_SERVER_CONFLICT_GRACE`] after it started or took the lock, then stops as
//!     before;
//!   * a respond answered 503 or 429 WITH a whole-seconds `Retry-After` that fits the
//!     remaining budget (the server wrote nothing) is re-sent a bounded number of times,
//!     all of it inside one deadline from the first send; every other non-204 (403 and
//!     413 included) is not retried. Each POST waits longer than the server's own
//!     delivery deadline, so the server's retryable `503 RESPONSE_NOT_DELIVERED` arrives
//!     and is retried ([`RESPOND_ATTEMPT_TIMEOUT`], [`RESPOND_RETRY_BUDGET`]); a retry is
//!     started only while a whole attempt still fits the budget after its wait, and
//!     every send but the first is made only if a whole attempt is still left
//!     immediately before it (else the audit says the last refusal seen). The
//!     request's audit entry is recorded as `undelivered` only when the refusal is the
//!     shed server's own (a complete API error body with an `error.code`, the last 503
//!     before the budget ran out included); any other status, and a POST that got no
//!     answer in time, keeps the operation's result, with "delivery unknown" in the
//!     reason;
//!   * redirects are followed only to the configured server's own HTTPS origin, a few
//!     hops at most, so the identity headers and the client certificate never reach
//!     another host.
//!
//! Scope: the daemon subscribes only to `ssh-agent` and implements the full
//! ssh-agent op set — the gated **`sign`** flow (decode → the signing check
//! ([`crate::sign_check`], plan 035 §3.5) → approval gate → backend → response + audit;
//! a request the check declines never reaches the gate and is audited `refused`), the
//! ungated **`list`** (backend keys → `SSHListResponse` + a durable non-gated audit) and
//! **`status`** (`{connected, mode, key_count}`) ops, and `ping` → `pong`. An unknown
//! operation gets Go's exact `unknown operation: <op>` `INTERNAL_ERROR` envelope, and
//! a payload that isn't a JSON object gets `{invalid payload, INTERNAL_ERROR}` — so the
//! shed's request never hangs. The `aws-credentials` and `docker-credentials` namespaces
//! are wired alongside (each its own per-namespace gate), and the always-on egress-audit
//! SSE consumer ([`crate::egress`]) runs as a side task per server — so the Go and Rust
//! daemons now touch the same endpoint set (the endpoint asymmetry closed with this slice).

use std::collections::{BTreeMap, HashMap};
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
use shed_core::tls::{
    pinned_client_config, pinned_client_config_with_client_auth, ClientCertResolver,
};

use crate::approval::ApprovalGate;
use crate::audit::{AuditEntry, AuditSink};
use crate::aws_backend::{aws_expiry_detail, aws_literal_z, AwsBackend};
use crate::config::{NS_AWS_CREDENTIALS, NS_DOCKER_CREDENTIALS, NS_SSH_AGENT};
use crate::discovery::ServerTarget;
use crate::docker_backend::{
    DockerBackend, DOCKER_CODE_INTERNAL, DOCKER_CODE_NOT_ALLOWED, DOCKER_CODE_NOT_FOUND,
};
use crate::machine::MachineIdentity;
use crate::presence::Presence;
use crate::sign_check::{Refusal, RefusalClass, SignPolicy};
use crate::ssh_backend::SshBackend;
use crate::status::ActiveSource;

/// The machine-identity request headers every subscribe and respond carries (plan 035
/// §3.4). The id is sent as is (32 lowercase hex); the name percent-encoded.
pub const HEADER_MACHINE_ID: &str = "X-Shed-Machine-Id";
pub const HEADER_MACHINE_NAME: &str = "X-Shed-Machine-Name";
/// `error.code` of the 403 a pinning server answers when another machine is this user's
/// credential source. The ONLY code that makes a subscription `not-source`.
pub const CODE_SOURCE_NOT_BOUND: &str = "SOURCE_NOT_BOUND";
/// `error.code` of the 409 a pinning server answers when a process with this same
/// machine id already holds the namespace. Retryable; a 409 without it stays terminal.
pub const CODE_SOURCE_DUPLICATE_STREAM: &str = "SOURCE_DUPLICATE_STREAM";
/// The longest a server-supplied machine name or id is kept for display (the server
/// itself accepts at most 63 bytes; the slack covers an id).
const MAX_SOURCE_FIELD_BYTES: usize = 64;
/// The longest server-supplied `error.code` copied into an audit reason.
const MAX_CODE_CHARS: usize = 64;

/// `error.code` of the 429 a pinning server answers a subscribe when this user has had
/// too many new or changed machines lately; it carries a whole-seconds `Retry-After`.
pub const CODE_SOURCE_RATE_LIMITED: &str = "SOURCE_RATE_LIMITED";
/// The longest a `429 SOURCE_RATE_LIMITED`'s `Retry-After` is honoured for.
const SUBSCRIBE_RETRY_AFTER_CAP: Duration = Duration::from_secs(60);

/// How often the daemon's one lock watcher ([`crate::presence::LockWatcher`]) looks
/// whether the desktop app's embedded broker runs on this machine. Every subscription of
/// a client with the watcher's channel ([`BusClient::with_presence`]) reacts to what it
/// publishes at once — while waiting, while connecting, while holding a stream, while
/// backing off, and just before posting an answer. A local syscall on a lock file, no
/// network: what bounds how soon the daemon takes over after the app quits, and how soon
/// it lets go after the app starts.
pub const PRESENCE_CHECK_INTERVAL: Duration = Duration::from_secs(2);
/// A `yielding` namespace's `last_error`: who answers, and when this client takes over.
pub const YIELDING_REASON: &str = "the desktop app on this machine is answering (its embedded \
     broker is running); this daemon takes over within a few seconds of the app quitting";
/// How long, from when it started subscribing (or took the presence lock, if later), a
/// client holding the presence lock ([`BusClient::with_presence_lock`]: the desktop's
/// embedded broker) retries a TERMINAL `409` (a 0.9.x server's
/// `NAMESPACE_ALREADY_REGISTERED`: another stream of this machine holds the type) instead
/// of stopping. On such a server the daemon may still hold the type when the app first
/// subscribes; it lets go at its next look, at most one [`PRESENCE_CHECK_INTERVAL`] away
/// (two allowing for a slow look), and the server unregisters its listener when the
/// connection closes. Four intervals (8 s) plus 7 s for that teardown and for the app's
/// backoff (1, 2, 4 s) to come round to a retry: 15 s. Past it, a 409 is terminal again,
/// as for every other client.
pub const OLD_SERVER_CONFLICT_GRACE: Duration =
    Duration::from_secs(4 * PRESENCE_CHECK_INTERVAL.as_secs() + 7);
/// The audit note of an answer the daemon did not post because the desktop app's embedded
/// broker took over the request type while the request was being handled: the server
/// re-delivers the still-pending request to the app, which answers it.
pub const AUDIT_NOTE_LEFT_FOR_APP: &str = "left for the desktop app";

/// How many times a respond answered 503/429 with `Retry-After` is re-sent.
const RESPOND_MAX_RETRIES: u32 = 3;
/// The shed server's own deadline for delivering one answer into the shed:
/// `responseTimeout` in `internal/plugin/bridge.go` (5 s), started when the server begins
/// matching the answer. Only once it has passed does the server answer
/// `503 RESPONSE_NOT_DELIVERED` with `Retry-After: 1` (`writeRespondError` in
/// `internal/api/plugin_handlers.go`), nothing written and the request still pending.
const SERVER_RESPONSE_DEADLINE_SECS: u64 = 5;
/// The `Retry-After` the server sends with that 503 (and with `429 TOO_MANY_RESPONSES`).
const SERVER_RESPOND_RETRY_AFTER_SECS: u64 = 1;
/// What one respond POST allows beyond the server's deadline: the request's way there,
/// the server reading and decoding its body before the deadline starts, and the answer's
/// way back.
const RESPOND_ATTEMPT_MARGIN_SECS: u64 = 3;
/// How long one respond POST waits for its answer: past the server's delivery deadline
/// with margin (5 + 3 = 8 s), so the server's own retryable 503 is what ends a slow
/// delivery, never this client's clock.
const RESPOND_ATTEMPT_TIMEOUT: Duration =
    Duration::from_secs(SERVER_RESPONSE_DEADLINE_SECS + RESPOND_ATTEMPT_MARGIN_SECS);
/// One absolute deadline, from the first respond send, for everything after it: every
/// refusal body read, every retry wait, every credential fetch and every send. It holds
/// one whole attempt, the server's `Retry-After`, and a second whole attempt (8 + 1 + 8
/// = 17 s), so an answer the server could not deliver within its deadline is sent at
/// least once more. Past it the namespace's serial handler is free again, and the audit
/// says what became of the answer.
const RESPOND_RETRY_BUDGET: Duration = Duration::from_secs(
    2 * (SERVER_RESPONSE_DEADLINE_SECS + RESPOND_ATTEMPT_MARGIN_SECS)
        + SERVER_RESPOND_RETRY_AFTER_SECS,
);
/// The shortest wait before a retried respond (a `Retry-After: 0` must not hot-loop).
const RESPOND_RETRY_MIN_WAIT: Duration = Duration::from_millis(100);
/// The longest a non-2xx body may take to arrive (subscribe and respond alike): a body
/// that never ends must not stall a namespace's loop until shutdown. A respond's read is
/// also cut at its deadline, whichever comes first.
const ERROR_BODY_READ_TIMEOUT: Duration = Duration::from_secs(3);
/// The most redirects the bus follows, and only within the configured server's origin.
const MAX_REDIRECTS: usize = 3;
/// The audit `result` of an operation whose response the server refused: whatever the
/// operation did, the shed never received it.
pub const AUDIT_RESULT_UNDELIVERED: &str = "undelivered";

/// SSH protocol error codes (`internal/ext/protocol/ssh.go`).
const SSH_CODE_KEY_NOT_FOUND: &str = "KEY_NOT_FOUND";
const SSH_CODE_SIGN_FAILED: &str = "SIGN_FAILED";
const SSH_CODE_INTERNAL: &str = "INTERNAL_ERROR";

/// The audit `result` of a sign request the signing check declined (plan 035 §3.5): it
/// never reached the approval gate or the backend.
pub const AUDIT_RESULT_REFUSED: &str = "refused";
/// The audit-only `code` of a refused sign request (like `APPROVAL_DENIED`, it is never
/// sent to the shed, which gets the ordinary failure shape).
pub const AUDIT_CODE_SIGN_REFUSED: &str = "SIGN_REFUSED";
/// The `reason` of the summary line written for refusals that were counted rather than
/// logged one by one ([`RefusalAuditLimiter`]).
pub const AUDIT_REASON_RATE_LIMITED: &str = "audit_rate_limited";
/// At most this many refusal lines per shed in one [`REFUSAL_AUDIT_WINDOW`] (plus the
/// first refusal of each class not yet logged in the window); the rest are counted per
/// class and written as one summary line when the window ends.
const REFUSAL_AUDIT_BURST: u32 = 20;
const REFUSAL_AUDIT_WINDOW: Duration = Duration::from_secs(60);
/// The most per-shed refusal windows kept at once: a hard cap. A new shed past it first
/// drops the windows that have ended, then evicts the oldest open one, writing out what
/// either had counted.
const REFUSAL_AUDIT_MAX_SHEDS: usize = 1024;
/// The number of refusal classes, each counted on its own in a window.
const REFUSAL_CLASSES: usize = RefusalClass::ALL.len();

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
    /// The bearer still held after `token()` failed — a failed re-mint keeps the
    /// prior credential presentable (Go parity: the server might still accept
    /// it, and presenting something beats presenting nothing). Default `None`
    /// (static providers never fail).
    fn surviving_token(&self) -> Option<String> {
        None
    }
    /// Whether a certificate identity is armed on the transport — used only to
    /// pick the right log line when `token()` fails. Default `false`.
    fn presenting_certificate(&self) -> bool {
        false
    }
    /// Whether [`Self::token`] would now start (or join) a mint rather than hand back a
    /// credential already held. A respond makes at most ONE such call: after it, the held
    /// credential is presented as it is ([`Self::surviving_token`], or the certificate
    /// still armed on the transport). Default `false` (a static provider never mints).
    fn would_mint(&self) -> bool {
        false
    }
    /// [`Self::token`] for one request; on failure also the refusal of the certificate
    /// the mint THIS call made or joined was issued (plan 037 §3.2.2), when that is why it
    /// failed. The request explains a server's refusal with it — never with a refusal
    /// another request's mint produced. Default: [`Self::token`], with no refusal.
    async fn token_for_request(&self) -> Result<String, (BusError, Option<MintRefusal>)> {
        self.token().await.map_err(|e| (e, None))
    }
}

/// The refusal of the certificate one request's own mint was issued
/// ([`TokenProvider::token_for_request`], plan 037 §3.2.2).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MintRefusal {
    pub message: String,
    /// The held-user rule's refusal (a certificate for another named user, or one that
    /// names no user: [`crate::minter::ObtainError::held_user`]). With nothing held to
    /// present, the request is then not sent at all (plan 037 A23); any other refusal
    /// only explains the server's answer to a request that is sent as before.
    pub held_user: bool,
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

/// The static bearer a server group that does not mint sends, on the bus and on the egress
/// stream: the entry's `credentials_token`, except for an entry that names an
/// `identity_file` (plan 037 A8), which never sends a stored token — it says nothing about
/// which SSH key it was issued for. Such a group sends no bearer.
pub(crate) fn static_bearer(target: &ServerTarget) -> String {
    if target.identity_file.is_empty() {
        target.token.clone()
    } else {
        String::new()
    }
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
/// loop instead of hot-looping a retry. `NotSource` and `Duplicate` (plan 035 §3.4) are
/// NOT terminal: the loop keeps retrying on its backoff. `Yielding` is not terminal
/// either: the loop stands by, subscribing nothing, until the embedded broker is gone.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ConnState {
    Reconnecting,
    Connected,
    Stopped,
    Rejected,
    /// The server answers this user's credential requests through another machine
    /// (`403 SOURCE_NOT_BOUND`); [`SubStatus::active_source`] names it.
    NotSource,
    /// Another process with this machine's id already holds the namespace on the server
    /// (`409 SOURCE_DUPLICATE_STREAM`) — e.g. two daemons on a copied state directory,
    /// or the daemon in the instant before it sees the desktop app's lock.
    Duplicate,
    /// The desktop app's embedded broker runs on this machine ([`crate::presence`]), so
    /// this client (the daemon) does not subscribe: the app answers. Only a client with
    /// a presence check ([`BusClient::with_presence`]) is ever in it.
    Yielding,
}

/// A snapshot of one subscription's state (`hostclient.go:SubStatus`). `since` is the
/// RFC3339-UTC instant the CURRENT state began — re-stamped only when the state actually
/// changes (mirror the Go SDK's `setState`), so a snapshot shows how long the subscription
/// has held its state. `servers[]` masks the `since` value (RFC3339-shape asserted first).
#[derive(Debug, Clone)]
pub struct SubStatus {
    pub namespace: String,
    pub state: ConnState,
    pub last_error: String,
    pub since: String,
    /// The machine the server named as the active source; `Some` only in
    /// [`ConnState::NotSource`].
    pub active_source: Option<ActiveSource>,
}

/// Record a namespace's connection state, stamping `since` only when the state actually
/// changes (a same-state call just refreshes `last_error`) — a faithful port of the Go
/// SDK's `HostClient.setState` (`hostclient.go:256`). Any state but `NotSource` clears
/// the active source.
fn set_state(status: &Arc<Mutex<SubStatus>>, state: ConnState, cause: Option<&str>) {
    set_state_with(status, state, cause, None);
}

fn set_state_with(
    status: &Arc<Mutex<SubStatus>>,
    state: ConnState,
    cause: Option<&str>,
    active_source: Option<ActiveSource>,
) {
    let mut s = status.lock().unwrap();
    s.last_error = cause.unwrap_or("").to_string();
    s.active_source = active_source;
    if s.state != state {
        s.state = state;
        s.since = crate::status::now_rfc3339();
    }
}

/// What a terminal-looking `409` means for a client ([`BusClient::with_presence_lock`]).
enum Grace {
    /// Terminal, as for every client without the presence lock.
    None,
    /// Inside the older-server grace: retry on the backoff.
    Retry,
    /// The client is the embedded broker but does not hold its lock yet: rejected until
    /// it does, then retried.
    WaitForLock(watch::Receiver<Option<tokio::time::Instant>>),
}

/// How a single `stream_messages` attempt ended, driving the reconnect decision.
enum StreamEnd {
    /// Shutdown was requested mid-connect / mid-stream — return cleanly.
    Shutdown,
    /// 409: another listener owns the namespace → terminal (no retry).
    Conflict(String),
    /// 409 `SOURCE_DUPLICATE_STREAM`: this machine's id already holds the namespace in
    /// another process → backoff + retry, no re-mint.
    Duplicate(String),
    /// 403 `SOURCE_NOT_BOUND`: another machine is the source → backoff + retry, no
    /// re-mint.
    NotSource {
        reason: String,
        source: ActiveSource,
    },
    /// 401: token rejected → invalidate + reconnect.
    Unauthorized(String),
    /// A connect/read transport error → backoff + retry.
    Transport(String),
    /// A non-{200,401,409} status → backoff + retry. The status code is already
    /// embedded in the message; no separate field.
    BadStatus(String),
    /// `429 SOURCE_RATE_LIMITED` → retry no sooner than the server's `Retry-After`
    /// (capped), and never sooner than the backoff.
    RateLimited {
        reason: String,
        retry_after: Option<Duration>,
    },
    /// The server closed a healthy stream → reconnect.
    ClosedByServer(String),
    /// This request's own mint was refused by the held-user rule and nothing is held to
    /// present (plan 037 A23): nothing was sent → backoff + retry, which mints again.
    NotSent(String),
    /// The desktop app's embedded broker appeared on this machine while this client
    /// was connecting or held the stream: the attempt was dropped (its connection
    /// closed) → stand by ([`BusClient::with_presence`]).
    Yield,
}

// ---------------------------------------------------------------------------
// Refusal classification (plan 035 §3.4) — pure, so the table is unit-tested
// ---------------------------------------------------------------------------

/// What a non-200 answer to a subscribe means.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SubscribeRefusal {
    /// 403 with `error.code == SOURCE_NOT_BOUND`: another machine is the source.
    NotSource(ActiveSource),
    /// 409 with `error.code == SOURCE_DUPLICATE_STREAM`: retryable.
    Duplicate,
    /// Any other 409: terminal, exactly as before the pin.
    Conflict,
    /// 401: re-mint, as before.
    Unauthorized,
    /// 429 with `error.code == SOURCE_RATE_LIMITED`: retried after the server's
    /// `Retry-After`.
    RateLimited,
    /// Everything else — a 403 with any other code or an unparseable body included —
    /// retries on the backoff with the server's body shown, as before.
    Other,
}

/// Classify a non-200 subscribe answer from its status and (at most 1024 bytes of) body.
/// `not-source` is entered if and only if the status is 403 AND the body was read
/// `complete` AND it parses as the API's error shape with `error.code ==
/// SOURCE_NOT_BOUND`. A body cut short (over the byte cap, too slow, a read error) is
/// never classified, even when the part read parses on its own: a 403 is then `Other`
/// and a 409 terminal.
pub fn classify_subscribe_refusal(status: u16, body: &str, complete: bool) -> SubscribeRefusal {
    let parsed = || {
        if complete {
            parse_api_error(body)
        } else {
            None
        }
    };
    match status {
        401 => SubscribeRefusal::Unauthorized,
        403 => match parsed() {
            Some((Some(code), v)) if code == CODE_SOURCE_NOT_BOUND => {
                SubscribeRefusal::NotSource(active_source_of(&v))
            }
            _ => SubscribeRefusal::Other,
        },
        409 => match parsed() {
            Some((Some(code), _)) if code == CODE_SOURCE_DUPLICATE_STREAM => {
                SubscribeRefusal::Duplicate
            }
            _ => SubscribeRefusal::Conflict,
        },
        429 => match parsed() {
            Some((Some(code), _)) if code == CODE_SOURCE_RATE_LIMITED => {
                SubscribeRefusal::RateLimited
            }
            _ => SubscribeRefusal::Other,
        },
        _ => SubscribeRefusal::Other,
    }
}

/// The wait a `429 SOURCE_RATE_LIMITED`'s `Retry-After` asks for: a whole number of
/// seconds, capped at [`SUBSCRIBE_RETRY_AFTER_CAP`]. Anything else (an HTTP date, a
/// fraction, garbage, no header) is `None`: the ordinary backoff applies.
fn subscribe_retry_after(value: Option<&reqwest::header::HeaderValue>) -> Option<Duration> {
    let raw = value?.to_str().ok()?.trim();
    if raw.is_empty() || !raw.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    // A number too large to hold is still "wait as long as allowed".
    let secs = raw.parse::<u64>().unwrap_or(u64::MAX);
    Some(Duration::from_secs(secs).min(SUBSCRIBE_RETRY_AFTER_CAP))
}

/// Parse an API error body (`{"error":{"code":…,"message":…}, …}`): the JSON value and
/// `error.code` when it is a string. `None` when the body is not a JSON object.
fn parse_api_error(body: &str) -> Option<(Option<String>, serde_json::Value)> {
    let v: serde_json::Value = serde_json::from_str(body).ok()?;
    if !v.is_object() {
        return None;
    }
    let code = v
        .get("error")
        .and_then(|e| e.get("code"))
        .and_then(|c| c.as_str())
        .map(str::to_string);
    Some((code, v))
}

/// The `error.message` of an API error body, if it has one.
fn api_error_message(body: &str) -> Option<String> {
    let (_, v) = parse_api_error(body)?;
    v.get("error")?
        .get("message")?
        .as_str()
        .map(|m| display_field(m, 256))
}

/// The `source` object of a `SOURCE_NOT_BOUND` refusal. Tolerant: a missing or
/// wrongly-typed field is empty. Both values are server-supplied and end up in logs and
/// status, so they are cleaned for display.
fn active_source_of(v: &serde_json::Value) -> ActiveSource {
    let field = |k: &str| {
        v.get("source")
            .and_then(|s| s.get(k))
            .and_then(|f| f.as_str())
            .map(|s| display_field(s, MAX_SOURCE_FIELD_BYTES))
            .unwrap_or_default()
    };
    ActiveSource {
        machine_id: field("machine_id"),
        machine_name: field("machine_name"),
    }
}

/// A server-supplied string made safe to log and show: control characters and Unicode
/// format characters dropped (so it can neither forge a line, send a terminal escape,
/// nor reorder or hide what is around it), cut to at most `max_bytes` on a character
/// boundary.
fn display_field(raw: &str, max_bytes: usize) -> String {
    let mut out = String::new();
    for c in raw
        .chars()
        .filter(|c| !c.is_control() && !is_format_char(*c))
    {
        if out.len() + c.len_utf8() > max_bytes {
            break;
        }
        out.push(c);
    }
    out
}

/// Unicode format characters (general category Cf): invisible, but they change how the
/// text around them is shown — bidirectional overrides, embeddings and isolates, the
/// Arabic letter mark, zero-width spaces and joiners, word joiners and invisible
/// operators, the byte-order mark, interlinear annotation marks, tag characters.
fn is_format_char(c: char) -> bool {
    matches!(
        c as u32,
        0x00AD
            | 0x0600..=0x0605
            | 0x061C
            | 0x06DD
            | 0x070F
            | 0x0890..=0x0891
            | 0x08E2
            | 0x180E
            | 0x200B..=0x200F
            | 0x202A..=0x202E
            | 0x2060..=0x206F
            | 0xFEFF
            | 0xFFF9..=0xFFFB
            | 0x110BD
            | 0x110CD
            | 0x13430..=0x1343F
            | 0x1BCA0..=0x1BCA3
            | 0x1D173..=0x1D17A
            | 0xE0001
            | 0xE0020..=0xE007F
    )
}

/// A server-supplied response body, as it may be shown in status or written to a log:
/// cleaned like [`display_field`] and capped at [`MAX_ERROR_BODY_BYTES`].
fn display_body(raw: &str) -> String {
    display_field(raw, MAX_ERROR_BODY_BYTES)
}

/// How this machine's name travels in [`HEADER_MACHINE_NAME`]: UTF-8, every byte that
/// is not an RFC 3986 unreserved character (`A-Z a-z 0-9 - . _ ~`) as `%XX`. The result
/// is plain ASCII, valid as a header value, and decodes the same under a path or a query
/// unescape (a space is `%20`, a `+` is `%2B`).
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

/// The text a person reads for a `not-source` subscription (status `last_error`, the
/// log). An old status reader with no `active_source` field still learns who is active.
fn not_source_reason(source: &ActiveSource) -> String {
    let who = match (source.machine_name.is_empty(), source.machine_id.is_empty()) {
        (false, false) => format!("{} ({})", source.machine_name, source.machine_id),
        (false, true) => source.machine_name.clone(),
        (true, false) => source.machine_id.clone(),
        (true, true) => "another machine".to_string(),
    };
    format!("not the credential source for this server; the active source is {who}")
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
    /// The server's config name, for the log lines ([`Self::server_label`]); empty
    /// until [`Self::with_server_name`] sets one (the single unnamed target keeps it
    /// empty, and is named by its URL instead).
    server_name: String,
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
    /// This machine's identity, sent on every subscribe and respond; `None` sends no
    /// identity headers at all (no identity could be loaded).
    machine: Option<MachineIdentity>,
    /// Bumped by [`Self::retry_now`]: every subscription of this client waiting out a
    /// backoff retries at once, with the backoff reset to the floor. A counter in a
    /// watch channel rather than a `Notify`, so a bump that lands while a subscription
    /// is mid-request is not lost: its next wait returns immediately.
    wake: Arc<watch::Sender<u64>>,
    /// Set by the daemon only: whether the desktop app's embedded broker runs on this
    /// machine, and the yield epoch, as the daemon's one lock watcher publishes them; while
    /// it runs, this client subscribes nothing and answers nothing, and what it received
    /// or began under an older epoch it never answers ([`Self::with_presence`]).
    presence: Option<watch::Receiver<Presence>>,
    /// Set on the copy a request's handler answers through ([`Self::for_handler`]): the
    /// yield epoch the handler started in. A respond whose epoch has been passed sends
    /// nothing. `None` (every other copy): the epoch when the respond starts.
    handler_epoch: Option<u64>,
    /// Set by the desktop's embedded broker only: when it took the presence lock (`None`
    /// until it has), for the older-server grace ([`Self::with_presence_lock`]).
    presence_lock: Option<watch::Receiver<Option<tokio::time::Instant>>>,
    /// How long one respond POST waits for an answer ([`RESPOND_ATTEMPT_TIMEOUT`]).
    respond_attempt_timeout: Duration,
    /// One respond's whole deadline from its first send ([`RESPOND_RETRY_BUDGET`]).
    respond_budget: Duration,
    /// Test seam: an in-memory server that answers this client's subscribes in place of
    /// HTTP, so the presence check and the backoff can be driven on tokio's paused clock.
    #[cfg(test)]
    sim: Option<Arc<tests::SimBus>>,
}

impl BusClient {
    /// Build a bus client for `server_url`. `static_token` is the open-mode static
    /// bearer (sent when non-empty and there's no provider); `token_provider`, when
    /// set, supplies a refreshing token and is the only path that 401-retries.
    /// `pin` (`sha256:<hex>`) installs the leaf-cert pin verifier on an https URL;
    /// a pin on a non-https URL is refused (fail-closed, mirroring
    /// `hostclient.go:applyTLSPin`). `client_certs`, when set, installs the credential
    /// source's client-certificate resolver so an `auth.mode: mtls` server is
    /// authenticated by the CERTIFICATE rather than a bearer header — see
    /// [`build_http_client`] for why it is optional rather than always-on.
    pub fn new(
        server_url: impl Into<String>,
        static_token: String,
        token_provider: Option<Arc<dyn TokenProvider>>,
        pin: Option<String>,
        client_certs: Option<Arc<ClientCertResolver>>,
        log: Arc<dyn BusLog>,
    ) -> Result<Self, BusError> {
        let server_url = server_url.into().trim_end_matches('/').to_string();
        let pin = pin.filter(|p| !p.is_empty());
        if pin.is_some() && !server_url.to_lowercase().starts_with("https://") {
            return Err(BusError::Config(format!(
                "TLS pin configured for a non-https URL {server_url}; refusing to send unpinned plaintext"
            )));
        }
        let http = build_http_client(&server_url, pin.as_deref(), client_certs)?;
        Ok(Self {
            server_url,
            server_name: String::new(),
            http,
            static_token,
            token_provider,
            log,
            initial_backoff: INITIAL_BACKOFF,
            max_backoff: MAX_BACKOFF,
            machine: None,
            wake: Arc::new(watch::channel(0u64).0),
            presence: None,
            handler_epoch: None,
            presence_lock: None,
            respond_attempt_timeout: RESPOND_ATTEMPT_TIMEOUT,
            respond_budget: RESPOND_RETRY_BUDGET,
            #[cfg(test)]
            sim: None,
        })
    }

    /// Stand by while the desktop app's embedded broker runs on this machine
    /// ([`crate::presence`]), as `presence` (the daemon's one lock watcher's channel)
    /// says: while it reads present, subscribe nothing (state [`ConnState::Yielding`]),
    /// close a stream held or being opened, leave a sleep at once, start no handler for a
    /// request already received, and post no answer.
    ///
    /// A yield is also PERMANENT for the work it interrupted: every request carries the
    /// yield epoch ([`Presence::epoch`]) of the stream it came on, and every answer the
    /// epoch its handler started in. A request from an older epoch is never handled and
    /// an answer from one is never sent (a first send or a re-send), even when the app
    /// has quit again by then; a stream opened under an older epoch is closed. Such a
    /// request stays pending on the server, which delivers it again to whichever stream
    /// of this machine subscribes next: the app's, or this daemon's new one.
    ///
    /// `None` (the default) never stands by. The daemon passes one; the embedded broker,
    /// the one that answers while it runs, passes none.
    pub fn with_presence(mut self, presence: Option<watch::Receiver<Presence>>) -> Self {
        self.presence = presence;
        self
    }

    /// This client holds the presence lock: `held_since` is when it took it (`None`
    /// until it has). Set only by the desktop's embedded broker. Its one effect: a
    /// terminal `409` (an older server's `NAMESPACE_ALREADY_REGISTERED`) is retried on
    /// the backoff for [`OLD_SERVER_CONFLICT_GRACE`] after it started subscribing or took
    /// the lock, whichever is later, because on such a server the daemon on this machine
    /// may still hold the type for a moment after the app takes the lock. A client
    /// without it treats every such 409 as terminal, as before.
    pub fn with_presence_lock(
        mut self,
        held_since: Option<watch::Receiver<Option<tokio::time::Instant>>>,
    ) -> Self {
        self.presence_lock = held_since;
        self
    }

    /// Whether the desktop app's embedded broker runs now, as the watcher last published
    /// (always `false` without one). A channel read: no file I/O, no wait.
    fn embedded_present(&self) -> bool {
        self.presence.as_ref().is_some_and(|p| p.borrow().present)
    }

    /// The yield epoch as the watcher last published it ([`Presence::epoch`]; always 0
    /// without a presence channel, so nothing is ever stale there).
    fn yield_epoch(&self) -> u64 {
        self.presence.as_ref().map_or(0, |p| p.borrow().epoch)
    }

    /// Whether work stamped with yield epoch `epoch` was overtaken by a start of the
    /// desktop app's embedded broker: it is then never handled or answered here.
    fn yielded_since(&self, epoch: u64) -> bool {
        self.yield_epoch() != epoch
    }

    /// The copy a request's handler answers through: its responds belong to yield epoch
    /// `epoch` (the epoch of the stream the request came on, checked current when the
    /// handler started).
    fn for_handler(&self, epoch: u64) -> BusClient {
        let mut c = self.clone();
        c.handler_epoch = Some(epoch);
        c
    }

    /// Present `machine` on every subscribe and respond. An identity whose id or name
    /// is not one this build would write is dropped (no headers), never sent half-valid.
    pub fn with_machine(mut self, machine: Option<MachineIdentity>) -> Self {
        self.machine = machine
            .filter(|m| crate::machine::valid_id(&m.id) && crate::machine::valid_name(&m.name));
        self
    }

    /// Cut every waiting backoff of this client's subscriptions short and reset it to
    /// the floor — what a "use this machine" move calls so the new source connects at
    /// once instead of within 30 s.
    pub fn retry_now(&self) {
        self.wake.send_modify(|n| *n = n.wrapping_add(1));
    }

    /// The handle [`Self::retry_now`] bumps, for a group that outlives this value.
    pub(crate) fn wake_handle(&self) -> Arc<watch::Sender<u64>> {
        self.wake.clone()
    }

    /// Add the machine-identity headers when this client has an identity.
    fn with_identity(&self, req: reqwest::RequestBuilder) -> reqwest::RequestBuilder {
        match &self.machine {
            Some(m) => req
                .header(HEADER_MACHINE_ID, m.id.as_str())
                .header(HEADER_MACHINE_NAME, percent_encode_name(&m.name)),
            None => req,
        }
    }

    /// Name the server this client talks to, for its log lines — the supervisor's
    /// `target.name`. A process that brokers several servers (the daemon, the desktop's
    /// embedded broker) writes one log for all of them, and a connect, a drop or a 409
    /// that does not say which server it was is ambiguous there.
    pub fn with_server_name(mut self, name: impl Into<String>) -> Self {
        self.server_name = name.into();
        self
    }

    /// The `server=` field of this client's log lines: its config name, or — for the
    /// unnamed single-server target, which has none — its base URL.
    fn server_label(&self) -> &str {
        if self.server_name.is_empty() {
            &self.server_url
        } else {
            &self.server_name
        }
    }

    /// Subscribe to `namespace`'s SSE stream. Spawns the reconnecting loop and
    /// returns a [`Subscription`] whose `rx` yields inbound envelopes and whose
    /// `status()` reports the live connection state. The loop runs until `shutdown`
    /// flips true or the subscription is terminally rejected (409).
    pub fn subscribe(&self, namespace: &str, shutdown: watch::Receiver<bool>) -> Subscription {
        let (tx, rx) = mpsc::channel::<Received>(ENVELOPE_CHANNEL_CAP);
        let status = Arc::new(Mutex::new(SubStatus {
            namespace: namespace.to_string(),
            state: ConnState::Reconnecting,
            last_error: String::new(),
            // Stamp the initial "reconnecting" instant (Go's `subscribeLoop` opens with a
            // `setState(ConnReconnecting)`); subsequent same-state calls keep it.
            since: crate::status::now_rfc3339(),
            active_source: None,
        }));
        let client = self.clone();
        let ns = namespace.to_string();
        let st = status.clone();
        // Subscribed here, not in the task, so a `retry_now` from the moment this
        // returns is seen.
        let wake = self.wake.subscribe();
        let handle = tokio::spawn(async move {
            client.subscribe_loop(ns, tx, shutdown, st, wake).await;
        });
        Subscription {
            rx: EnvelopeRx { rx },
            status,
            handle,
        }
    }

    async fn subscribe_loop(
        self,
        namespace: String,
        tx: mpsc::Sender<Received>,
        shutdown: watch::Receiver<bool>,
        status: Arc<Mutex<SubStatus>>,
        mut wake: watch::Receiver<u64>,
    ) {
        let mut backoff = self.initial_backoff;
        let mut down_logged = false;
        // Terminal state defaults to a clean stop; a 409 overrides it to Rejected so
        // the final snapshot surfaces the rejection (mirrors hostclient.go's deferred
        // `setState(stopState, stopCause)`).
        let mut terminal_state = ConnState::Stopped;
        let mut terminal_cause: Option<String> = None;
        // When this subscription started: the older-server grace counts from here (or
        // from when the presence lock was taken, if later).
        let started = tokio::time::Instant::now();
        let mut grace_said = false;
        loop {
            if *shutdown.borrow() {
                break;
            }
            // Before EVERY attempt: while the desktop app's embedded broker runs on this
            // machine, subscribe nothing (it answers); once it is gone, ask at once, from
            // the backoff floor.
            if self.embedded_present() {
                if !self
                    .stand_by(&namespace, &shutdown, &status, &mut down_logged)
                    .await
                {
                    break;
                }
                backoff = self.initial_backoff;
                continue;
            }
            // tokio's clock (the real one outside tests), so a paused-clock test sees
            // how long a stream held.
            let start = tokio::time::Instant::now();
            let connected = AtomicBool::new(false);
            // The yield epoch this stream belongs to: every request it delivers is stamped
            // with it.
            let epoch = self.yield_epoch();
            // The attempt races the presence watch: the embedded broker appearing while
            // it connects or holds a stream ends it (dropping the stream closes the
            // connection), and so does the epoch moving on (the app started and quit
            // again between two looks at the channel). Requests already received are not
            // answered by this daemon either: `serve_namespace` starts no handler for
            // them (and drops those queued when the app starts), and a handler already
            // running posts nothing (`send_respond`); each stays pending on the server,
            // which delivers it again to the next stream of this machine: the app's, or
            // this daemon's next one, opened under the new epoch.
            let end = tokio::select! {
                end = self.stream_messages(&namespace, &tx, epoch, shutdown.clone(), &status, &connected) => end,
                _ = self.embedded_appears_since(epoch) => StreamEnd::Yield,
            };
            let connected = connected.load(Ordering::SeqCst);
            // A wait the server asked for (a 429's `Retry-After`), instead of the backoff.
            let mut server_wait: Option<Duration> = None;

            match end {
                StreamEnd::Shutdown => break,
                StreamEnd::Yield => {
                    self.log.info(&format!(
                        "the desktop app's embedded broker started on this machine; closing \
                         namespace={namespace} server={} and leaving it to the app",
                        self.server_label()
                    ));
                    // The top of the loop stands by (or, if the app is already gone again,
                    // asks at once).
                    down_logged = false;
                    backoff = self.initial_backoff;
                    continue;
                }
                StreamEnd::Conflict(reason) => {
                    match self.conflict_grace(started) {
                        // The embedded broker, inside its grace: on an older server the
                        // daemon on this machine may hold the type for a moment after
                        // this app took the lock. Retry on the backoff.
                        Grace::Retry => {
                            let line = format!(
                                "namespace {namespace} server={} is held by another stream; \
                                 on an older server that is the shed-host-agent daemon on \
                                 this machine, which lets go within seconds of this app \
                                 taking its lock, so retrying backoff={backoff:?}: {reason}",
                                self.server_label()
                            );
                            if grace_said {
                                self.log.debug(&line);
                            } else {
                                self.log.info(&line);
                                grace_said = true;
                            }
                            set_state(&status, ConnState::Reconnecting, Some(&reason));
                            down_logged = false;
                        }
                        // The embedded broker before it holds the lock: the daemon does
                        // not know to let go, so this is the old terminal refusal — until
                        // the lock is taken, when the daemon will let go: then retry at
                        // once, the grace counting from that moment.
                        Grace::WaitForLock(mut held) => {
                            self.log.error(&format!(
                                "SSE subscription rejected; another listener owns namespace {namespace} server={} — \
                                 not retrying until this app holds its lock: {reason}",
                                self.server_label()
                            ));
                            set_state(&status, ConnState::Rejected, Some(&reason));
                            tokio::select! {
                                _ = wait_shutdown(shutdown.clone()) => {
                                    terminal_state = ConnState::Rejected;
                                    terminal_cause = Some(reason);
                                    break;
                                }
                                held_now = async { held.wait_for(|h| h.is_some()).await.is_ok() } => {
                                    if !held_now {
                                        // The holder is gone (the broker is stopping).
                                        wait_shutdown(shutdown.clone()).await;
                                        terminal_state = ConnState::Rejected;
                                        terminal_cause = Some(reason);
                                        break;
                                    }
                                }
                            }
                            set_state(&status, ConnState::Reconnecting, Some(&reason));
                            backoff = self.initial_backoff;
                            down_logged = false;
                            continue;
                        }
                        // Terminal: a second broker must be observably rejected, not
                        // silently retry forever (hostclient.go's errSubscribeConflict).
                        Grace::None => {
                            self.log.error(&format!(
                                "SSE subscription rejected; another listener owns namespace {namespace} server={} — not retrying: {reason}",
                                self.server_label()
                            ));
                            terminal_state = ConnState::Rejected;
                            terminal_cause = Some(reason);
                            break;
                        }
                    }
                }
                StreamEnd::NotSource { reason, source } => {
                    // Not terminal and not a credential problem: no invalidate (a
                    // re-mint per retry would be an SSH round trip every 30 s for
                    // nothing). Said once at INFO on entering the state (or when the
                    // active machine changes), then quietly at DEBUG.
                    let fresh = {
                        let s = status.lock().unwrap();
                        s.state != ConnState::NotSource || s.active_source.as_ref() != Some(&source)
                    };
                    let line = format!(
                        "{reason}; retrying namespace={namespace} server={} backoff={backoff:?}",
                        self.server_label()
                    );
                    if fresh {
                        self.log.info(&line);
                    } else {
                        self.log.debug(&line);
                    }
                    set_state_with(&status, ConnState::NotSource, Some(&reason), Some(source));
                    down_logged = false;
                }
                StreamEnd::Duplicate(reason) => {
                    let fresh = status.lock().unwrap().state != ConnState::Duplicate;
                    let line = format!(
                        "{reason}; retrying namespace={namespace} server={} backoff={backoff:?}",
                        self.server_label()
                    );
                    if fresh {
                        self.log.warn(&line);
                    } else {
                        self.log.debug(&line);
                    }
                    set_state(&status, ConnState::Duplicate, Some(&reason));
                    down_logged = false;
                }
                StreamEnd::Unauthorized(reason) => {
                    // The reason already leads with this request's own held-user refusal,
                    // when its mint was refused (`stream_messages`).
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
                | StreamEnd::ClosedByServer(reason)
                | StreamEnd::NotSent(reason) => {
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
                StreamEnd::RateLimited {
                    reason,
                    retry_after,
                } => {
                    set_state(&status, ConnState::Reconnecting, Some(&reason));
                    // The server's word on when to ask again, never sooner than the
                    // backoff would have waited anyway.
                    server_wait = retry_after.map(|r| r.max(backoff));
                    self.log_down(
                        &namespace,
                        &reason,
                        server_wait.unwrap_or(backoff),
                        &mut down_logged,
                    );
                }
            }

            match server_wait {
                // A wait the server asked for is waited out whole: a `retry_now` does not
                // cut it short (asking sooner is what the server just refused). A wake
                // that lands meanwhile is spent by the retry that follows.
                Some(wait) => {
                    tokio::select! {
                        _ = wait_shutdown(shutdown.clone()) => break,
                        _ = tokio::time::sleep(wait) => {}
                    }
                    wake.borrow_and_update();
                }
                // Back off — the backoff as it stands NOW, after any reset above — but
                // wake immediately on shutdown, retry at once (from the floor) on a
                // `retry_now`, and stand by at once when the desktop app's embedded broker
                // appears (the top of the loop does; when it is gone again, the
                // subscribe is at once, from the floor, not after the rest of this sleep).
                None => {
                    tokio::select! {
                        _ = wait_shutdown(shutdown.clone()) => break,
                        _ = tokio::time::sleep(backoff) => {}
                        _ = woken(&mut wake) => {
                            backoff = self.initial_backoff;
                            continue;
                        }
                        _ = self.embedded_appears() => {
                            backoff = self.initial_backoff;
                            continue;
                        }
                    }
                }
            }
            backoff = (backoff * 2).min(self.max_backoff);
        }
        set_state(&status, terminal_state, terminal_cause.as_deref());
        // Dropping `tx` here closes the receiver, ending the responder loop.
    }

    /// Stand by while the desktop app's embedded broker runs: state `yielding`, a look
    /// every [`PRESENCE_CHECK_INTERVAL`], nothing subscribed. Returns `true` once it is
    /// gone, `false` on shutdown. Said once at INFO on entering and once on leaving.
    async fn stand_by(
        &self,
        namespace: &str,
        shutdown: &watch::Receiver<bool>,
        status: &Arc<Mutex<SubStatus>>,
        down_logged: &mut bool,
    ) -> bool {
        self.log.info(&format!(
            "the desktop app's embedded broker is running on this machine; standing by \
             namespace={namespace} server={} (the app answers; this daemon takes over when it quits)",
            self.server_label()
        ));
        set_state(status, ConnState::Yielding, Some(YIELDING_REASON));
        if let Some(p) = &self.presence {
            let mut p = p.clone();
            tokio::select! {
                _ = wait_shutdown(shutdown.clone()) => return false,
                gone = async { p.wait_for(|v| !v.present).await.is_ok() } => {
                    if !gone {
                        // The watcher is gone (the daemon is stopping): stay put.
                        wait_shutdown(shutdown.clone()).await;
                        return false;
                    }
                }
            }
        }
        self.log.info(&format!(
            "the desktop app's embedded broker is gone; taking over namespace={namespace} server={}",
            self.server_label()
        ));
        set_state(status, ConnState::Reconnecting, None);
        *down_logged = false;
        true
    }

    /// Resolve once the watcher says the desktop app's embedded broker runs (at once if
    /// it says so now); never without a presence channel, or once the watcher is gone.
    async fn embedded_appears(&self) {
        let Some(p) = &self.presence else {
            return std::future::pending().await;
        };
        let mut p = p.clone();
        if p.wait_for(|v| v.present).await.is_err() {
            std::future::pending::<()>().await;
        }
    }

    /// Resolve with the yield epoch once it has moved past `seen` (the desktop app's
    /// embedded broker started); never without a presence channel, or once the watcher is
    /// gone.
    async fn next_yield(&self, seen: u64) -> u64 {
        let Some(p) = &self.presence else {
            return std::future::pending().await;
        };
        let mut p = p.clone();
        let epoch = p.wait_for(|v| v.epoch != seen).await.map(|v| v.epoch);
        match epoch {
            Ok(epoch) => epoch,
            Err(_) => std::future::pending().await,
        }
    }

    /// [`Self::embedded_appears`], or the yield epoch moving past `epoch` (the app started
    /// and may have quit again before this looked): what ends a stream of that epoch.
    async fn embedded_appears_since(&self, epoch: u64) {
        let Some(p) = &self.presence else {
            return std::future::pending().await;
        };
        let mut p = p.clone();
        if p.wait_for(|v| v.present || v.epoch != epoch).await.is_err() {
            std::future::pending::<()>().await;
        }
    }

    /// What a terminal-looking `409` means for this client ([`Self::with_presence_lock`]).
    fn conflict_grace(&self, started: tokio::time::Instant) -> Grace {
        let Some(lock) = &self.presence_lock else {
            return Grace::None;
        };
        let held_at = *lock.borrow();
        match held_at {
            None => Grace::WaitForLock(lock.clone()),
            Some(at)
                if tokio::time::Instant::now() < at.max(started) + OLD_SERVER_CONFLICT_GRACE =>
            {
                Grace::Retry
            }
            Some(_) => Grace::None,
        }
    }

    /// Loud-on-the-down-transition, quiet-while-down logging (mirrors
    /// `hostclient.go`): a persistently-unreachable server logs the WARN once, then
    /// drops to DEBUG (suppressed) — no per-cycle flood.
    fn log_down(&self, namespace: &str, reason: &str, backoff: Duration, down_logged: &mut bool) {
        let server = self.server_label();
        if !*down_logged {
            self.log.warn(&format!(
                "SSE connection lost, reconnecting namespace={namespace} server={server} backoff={backoff:?} error={reason}"
            ));
            *down_logged = true;
        } else {
            self.log.debug(&format!(
                "SSE still down, retrying namespace={namespace} server={server} backoff={backoff:?} error={reason}"
            ));
        }
    }

    async fn stream_messages(
        &self,
        namespace: &str,
        tx: &mpsc::Sender<Received>,
        epoch: u64,
        shutdown: watch::Receiver<bool>,
        status: &Arc<Mutex<SubStatus>>,
        connected: &AtomicBool,
    ) -> StreamEnd {
        #[cfg(test)]
        if let Some(sim) = &self.sim {
            // The credential is fetched as the real path below fetches it.
            let _ = tokio::select! {
                _ = wait_shutdown(shutdown.clone()) => return StreamEnd::Shutdown,
                t = self.bearer() => t,
            };
            let _ = (tx, epoch);
            return sim
                .subscribe(self, namespace, &shutdown, status, connected)
                .await;
        }
        let url = format!(
            "{}/api/plugins/listeners/{}/messages",
            self.server_url, namespace
        );
        let mut req = self.with_identity(
            self.http
                .get(&url)
                .header(reqwest::header::ACCEPT, "text/event-stream"),
        );
        // The bearer fetch can itself be a full SSH mint (after an
        // invalidation), so it races shutdown like every other network await —
        // a SIGTERM during a re-mint must not hold teardown for the mint
        // timeout.
        let (bearer, refusal) = tokio::select! {
            _ = wait_shutdown(shutdown.clone()) => return StreamEnd::Shutdown,
            t = self.bearer_for_request() => match t {
                Ok(t) => t,
                // Refused for its user with nothing to present: not sent (plan 037 A23).
                Err(refusal) => return StreamEnd::NotSent(refusal),
            },
        };
        if let Some(tok) = bearer {
            req = req.bearer_auth(tok);
        }
        // When this request's own mint was refused for its certificate's user, that is
        // why the server refuses it: say it first (plan 037 §3.2.2).
        let unauthorized = |reason: String| {
            StreamEnd::Unauthorized(match &refusal {
                Some(refusal) => format!("{refusal} (request refused: {reason})"),
                None => reason,
            })
        };

        // Race the connect against shutdown so a hung dial can't block teardown.
        let resp = tokio::select! {
            _ = wait_shutdown(shutdown.clone()) => return StreamEnd::Shutdown,
            r = req.send() => match r {
                Ok(r) => r,
                // A connect failure can BE the credential rejection: an
                // mtls-mode server refuses a bad certificate at (TLS 1.2) or
                // just after (TLS 1.3) the handshake, so there is no response
                // to carry a 401 — and reqwest's Display drops the TLS alert
                // entirely (it sits 2–3 levels down source()), so the error
                // must be FLATTENED before it can be classified or logged.
                // An auth-shaped failure takes the Unauthorized arm so the
                // reconnect loop re-mints instead of replaying the refused
                // credential forever (Go: streamMessages'
                // invalidateOnAuthFailure(0, err)).
                Err(e) => {
                    let reason = format!("connecting: {}", shed_core::authfail::flatten(&e));
                    return if shed_core::authfail::is_auth_shaped_message(&reason) {
                        unauthorized(reason)
                    } else {
                        StreamEnd::Transport(reason)
                    };
                }
            }
        };

        let st = resp.status().as_u16();
        if st != 200 {
            let retry_after = resp.headers().get(reqwest::header::RETRY_AFTER).cloned();
            // Bounded in bytes AND time, and shutdown-raced (Go's io.LimitReader 1024):
            // a hostile/huge/never-ending non-200 body can't blow up memory, stall this
            // namespace's loop, or block teardown. Only a complete body is classified;
            // only a cleaned one is shown.
            let read = read_error_body(resp, &shutdown, None).await;
            let body = read.shown();
            return match classify_subscribe_refusal(st, &read.raw, read.complete) {
                SubscribeRefusal::Conflict => StreamEnd::Conflict(format!(
                    "namespace already has an active subscriber: {body}"
                )),
                SubscribeRefusal::Duplicate => StreamEnd::Duplicate(format!(
                    "another process with this machine's id already holds namespace {namespace} \
                     on this server (duplicate stream){}",
                    api_error_message(&read.raw)
                        .map(|m| format!(": {m}"))
                        .unwrap_or_default()
                )),
                SubscribeRefusal::NotSource(source) => StreamEnd::NotSource {
                    reason: not_source_reason(&source),
                    source,
                },
                SubscribeRefusal::Unauthorized => {
                    unauthorized(format!("unexpected status 401: {body}"))
                }
                SubscribeRefusal::RateLimited => StreamEnd::RateLimited {
                    reason: format!("unexpected status {st}: {body}"),
                    retry_after: subscribe_retry_after(retry_after.as_ref()),
                },
                SubscribeRefusal::Other => {
                    StreamEnd::BadStatus(format!("unexpected status {st}: {body}"))
                }
            };
        }

        // Connected: reset the down-log, record the state, and stream envelopes.
        connected.store(true, Ordering::SeqCst);
        set_state(status, ConnState::Connected, None);
        self.log.info(&format!(
            "SSE connected namespace={namespace} server={}",
            self.server_label()
        ));

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
                                    r = tx.send(Received { env, epoch }) => r,
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
    /// promptly when it fires. Everything after the first send also runs against one
    /// deadline ([`RESPOND_RETRY_BUDGET`]), so no server can hold it longer.
    pub async fn respond(
        &self,
        namespace: &str,
        env: &Envelope,
        shutdown: &watch::Receiver<bool>,
    ) -> Result<(), BusError> {
        self.respond_detailed(namespace, env, shutdown)
            .await
            .map_err(|f| f.err)
    }

    /// [`Self::respond`], keeping what an audit needs to know about a refusal: whether it
    /// was the shed server's own.
    async fn respond_detailed(
        &self,
        namespace: &str,
        env: &Envelope,
        shutdown: &watch::Receiver<bool>,
    ) -> Result<(), RespondFailure> {
        let url = format!(
            "{}/api/plugins/listeners/{}/respond",
            self.server_url, namespace
        );
        let body = serde_json::to_vec(env).map_err(|e| {
            RespondFailure::from(BusError::Transport(format!("marshaling response: {e}")))
        })?;

        // A 503 or 429 that carries `Retry-After` means the server wrote nothing and
        // asks to be tried again (a busy delivery path, too many answers in flight), so
        // the answer is re-sent a bounded number of times, inside the deadline. Every
        // other non-204 — 403 (a stale or unmatched answer, or not the source) and 413
        // (too large) among them — is final.
        // The yield epoch this answer belongs to: its handler's, or now.
        let mut ctx = RespondCtx {
            epoch: self.handler_epoch.unwrap_or_else(|| self.yield_epoch()),
            ..RespondCtx::default()
        };
        let mut retries = 0u32;
        loop {
            let resp = match self.respond_attempt(&url, &body, shutdown, &mut ctx).await {
                Ok(resp) => resp,
                Err(err) => {
                    return Err(RespondFailure {
                        err,
                        server_code: None,
                        unanswered: ctx.unanswered,
                        not_sent: ctx.not_sent,
                        last_refusal: ctx.last_refusal.take(),
                        maybe_delivered: ctx.maybe_delivered,
                    })
                }
            };
            let st = resp.status().as_u16();
            if st == 204 {
                return Ok(());
            }
            let retry_after = resp.headers().get(reqwest::header::RETRY_AFTER).cloned();
            // Bounded in bytes and time (and by the deadline), shutdown-raced.
            let read = read_error_body(resp, shutdown, ctx.deadline).await;
            ctx.saw_refusal(st, read.server_code());
            let failed = RespondFailure {
                err: BusError::BadStatus(st, read.shown()),
                server_code: read.server_code(),
                unanswered: false,
                not_sent: None,
                last_refusal: None,
                maybe_delivered: ctx.maybe_delivered,
            };
            // Eligibility is decided AFTER the body read, against what is left of the
            // deadline then: a retry is started only if, after its wait, a whole attempt
            // (the server's delivery deadline and the margin) still fits. Otherwise the
            // respond ends on this refusal, which is the server's own word on the answer
            // (`undelivered` in the audit), rather than on an attempt cut short by the
            // budget, which could say nothing.
            let remaining = ctx.deadline.map_or(Duration::ZERO, |d| {
                d.saturating_duration_since(tokio::time::Instant::now())
            });
            let Some(wait) = respond_retry_wait(
                st,
                retry_after.as_ref(),
                retries,
                remaining,
                self.respond_attempt_timeout,
            ) else {
                return Err(failed);
            };
            retries += 1;
            self.log.info(&format!(
                "response not accepted yet; retrying in {wait:?} namespace={namespace} server={} \
                 status={st} attempt={retries}/{RESPOND_MAX_RETRIES}",
                self.server_label()
            ));
            tokio::select! {
                _ = wait_shutdown(shutdown.clone()) => return Err(failed),
                _ = tokio::time::sleep(wait) => {}
            }
        }
    }

    /// One delivery of a response body, with the credential handling `respond` has
    /// always had: one plain re-send on a lost pooled connection, and one invalidation (a
    /// TLS alert or a 401) per call to [`Self::respond`] — `ctx.reminted` carries that
    /// across the 503/429 retries so a refused fresh credential never earns a second one.
    async fn respond_attempt(
        &self,
        url: &str,
        body: &[u8],
        shutdown: &watch::Receiver<bool>,
        ctx: &mut RespondCtx,
    ) -> Result<reqwest::Response, BusError> {
        let mut sent = self.send_respond(url, body, shutdown, ctx).await;
        // Rust-only pool race (shed-core finding 29): a pooled connection the
        // server closed between checkout and write surfaces as "connection was
        // not ready" with no TLS alert anywhere in the chain. Nothing was
        // written, so ONE plain re-send is safe — the fresh connection either
        // works or produces the deterministic alert classified below.
        if matches!(&sent, Err(BusError::Transport(m))
            if shed_core::authfail::is_connection_lost_message(m))
        {
            sent = self.send_respond(url, body, shutdown, ctx).await;
        }
        // A credential can be refused mid-session in TWO shapes, and both must
        // invalidate + retry (one re-mint / two sends total, mirroring the Go
        // hostclient.go:Respond bound): a TLS-alert transport failure — the
        // mtls rejection has no HTTP status at all — and a provider-backed
        // 401 (token mode, or mtls per-request re-validation). A fresh
        // credential the server just refused does not earn a second mint.
        let mut resp = match sent {
            Ok(r) => r,
            Err(e) => {
                let auth_shaped = matches!(&e, BusError::Transport(m)
                    if shed_core::authfail::is_auth_shaped_message(m));
                match (&self.token_provider, auth_shaped, ctx.reminted) {
                    (Some(p), true, false) => {
                        p.invalidate();
                        ctx.reminted = true;
                        self.send_respond(url, body, shutdown, ctx).await?
                    }
                    _ => return Err(e),
                }
            }
        };
        if resp.status().as_u16() == 401 && !ctx.reminted {
            if let Some(p) = &self.token_provider {
                // The server's word on this answer so far (it wrote nothing): what the
                // audit says if the re-send cannot go out in time.
                let read = read_error_body(resp, shutdown, ctx.deadline).await;
                ctx.saw_refusal(401, read.server_code());
                p.invalidate();
                ctx.reminted = true;
                resp = self.send_respond(url, body, shutdown, ctx).await?;
            }
        }
        Ok(resp)
    }

    /// One POST. The first one starts the respond's deadline (after its own credential
    /// fetch, which is as it always was); every later one — its credential fetch and its
    /// send — must finish by that deadline. Each POST also waits at most
    /// [`RESPOND_ATTEMPT_TIMEOUT`] for its answer; one that got none is marked
    /// `ctx.unanswered` (it may or may not have been delivered).
    async fn send_respond(
        &self,
        url: &str,
        body: &[u8],
        shutdown: &watch::Receiver<bool>,
        ctx: &mut RespondCtx,
    ) -> Result<reqwest::Response, BusError> {
        let budget = self.respond_budget;
        let past_deadline = || {
            BusError::Transport(format!(
                "sending response: not answered within {budget:?} of the first send"
            ))
        };
        let mut req = self.with_identity(
            self.http
                .post(url)
                .header(reqwest::header::CONTENT_TYPE, "application/json")
                .body(body.to_vec()),
        );
        // The bearer fetch can itself be a full SSH mint — this is exactly the
        // call that re-mints after respond()'s invalidate — so it races
        // shutdown (a TLS rejection followed by SIGTERM must not hold teardown
        // for the mint timeout) and, after the first send, the deadline.
        let deadline = ctx.deadline;
        let bearer = tokio::select! {
            _ = wait_shutdown(shutdown.clone()) => {
                return Err(BusError::Transport("sending response: shutting down".into()))
            }
            t = until(deadline, self.respond_bearer(&mut ctx.mint_spent)) => match t {
                Some(Ok(t)) => t,
                // Refused for its user with nothing to present: not sent (plan 037 A23),
                // and the refusal is the whole error.
                Some(Err(refusal)) => return Err(BusError::Config(refusal)),
                None => {
                    ctx.not_sent = Some(NotSent::NoRoom);
                    return Err(past_deadline());
                }
            }
        };
        if let Some(tok) = bearer {
            req = req.bearer_auth(tok);
        }
        // Immediately before the send, two checks, each of which means nothing is sent:
        //
        // 1. The desktop app's embedded broker took over this request type on this
        //    machine (the daemon's watcher says so) while the request was being handled,
        //    or did at any moment since the handler started (the yield epoch moved on,
        //    even if the app has quit again since): the request belongs to whoever
        //    answered after that yield (the server re-delivers it to the app's stream,
        //    and, if it is still pending, to this daemon's next one), so this answer is
        //    never posted, as a first send or a re-send. Only a POST already on the wire
        //    can still overlap.
        if self.embedded_present() || self.yielded_since(ctx.epoch) {
            ctx.not_sent = Some(NotSent::LeftForApp);
            return Err(BusError::Transport(
                "sending response: not sent; the desktop app's embedded broker on this \
                 machine took over this request type"
                    .into(),
            ));
        }
        let now = tokio::time::Instant::now();
        let attempt = self.respond_attempt_timeout;
        // 2. Every send but the first must have a whole attempt left before the deadline
        //    (credential work, a 401's re-mint, a lost pooled connection or a retry wait
        //    may have used up the room): one the deadline would cut short could say
        //    nothing about the answer.
        if let Some(deadline) = ctx.deadline {
            if deadline.saturating_duration_since(now) < attempt {
                ctx.not_sent = Some(NotSent::NoRoom);
                return Err(BusError::Transport(format!(
                    "sending response: not sent again; less than {attempt:?} is left of the \
                     {budget:?} from the first send"
                )));
            }
        }
        let deadline = *ctx.deadline.get_or_insert(now + budget);
        let attempt_ends = (now + attempt).min(deadline);
        // A new POST: what it comes to is unknown until it is answered.
        ctx.last_refusal = None;
        // Race the POST against shutdown: a server that accepts the connection and
        // never replies must not block the daemon's teardown.
        tokio::select! {
            _ = wait_shutdown(shutdown.clone()) => {
                Err(BusError::Transport("sending response: shutting down".into()))
            }
            r = tokio::time::timeout_at(attempt_ends, req.send()) => match r {
                Err(_) => {
                    ctx.unanswered = true;
                    ctx.maybe_delivered = true;
                    Err(if attempt_ends < deadline {
                        BusError::Transport(format!(
                            "sending response: not answered within {attempt:?}"
                        ))
                    } else {
                        past_deadline()
                    })
                }
                // Flattened, not `{e}`: reqwest's Display drops the TLS alert
                // that respond()'s classifier (and any human reading the log)
                // needs — it sits down the source() chain.
                Ok(r) => r.map_err(|e| {
                    let flat = shed_core::authfail::flatten(&e);
                    // A failure that may come after the request was written could have
                    // reached the shed; a lost pooled connection (nothing written) or a
                    // refused credential (the server read nothing) could not.
                    if !shed_core::authfail::is_connection_lost_message(&flat)
                        && !shed_core::authfail::is_auth_shaped_message(&flat)
                    {
                        ctx.maybe_delivered = true;
                    }
                    BusError::Transport(format!("sending response: {flat}"))
                }),
            }
        }
    }

    /// The bearer for one respond POST, making at most ONE credential fetch that could
    /// mint per respond (`mint_spent`). After it — whether the mint succeeded or failed —
    /// the held credential is presented as it is: the surviving bearer, or nothing (the
    /// certificate still armed on the transport). Without this bound a failed re-mint
    /// leaves the source refused, and every 503 retry would mint again.
    ///
    /// `Err` is [`Self::bearer_for_request`]'s: the POST is not sent.
    async fn respond_bearer(&self, mint_spent: &mut bool) -> Result<Option<String>, String> {
        if let Some(p) = &self.token_provider {
            if p.would_mint() {
                if *mint_spent {
                    return Ok(p.surviving_token());
                }
                *mint_spent = true;
            }
        }
        self.bearer_for_request().await.map(|(bearer, _)| bearer)
    }

    /// The bearer token to send, or `None`. A provider takes precedence (and on a
    /// mint error the client sends unauthenticated, warning once — Go's setAuth);
    /// otherwise the static token is sent when non-empty; else no header.
    #[cfg(test)]
    async fn bearer(&self) -> Option<String> {
        self.bearer_for_request()
            .await
            .map(|(bearer, _)| bearer)
            .unwrap_or(None)
    }

    /// The bearer for one request, and the refusal of the certificate the mint THIS
    /// fetch made or joined was issued, when that is why the fetch failed (plan 037
    /// §3.2.2). The request carries it to its own outcome: a server's refusal of this
    /// request is explained by this mint's refusal and by no other.
    ///
    /// `Err(refusal)` when that refusal is the held-user rule's
    /// ([`MintRefusal::held_user`]) and nothing is held to present (no surviving token,
    /// no armed certificate): the request is not sent at all, since the server issues
    /// certificates and could not authenticate it, and the refusal is its whole error
    /// (plan 037 A23). Every other failure is sent as before.
    async fn bearer_for_request(&self) -> Result<(Option<String>, Option<String>), String> {
        if let Some(p) = &self.token_provider {
            match p.token_for_request().await {
                Ok(t) => Ok(((!t.is_empty()).then_some(t), None)),
                // A failed re-mint does not strip the wire of a credential the
                // server might still accept: present the surviving token (or,
                // in mtls state, the still-armed certificate) and log WHY the
                // replacement could not be minted (Go setAuth parity).
                Err((e, refusal)) => {
                    let bearer = if let Some(t) = p.surviving_token() {
                        self.log.warn(&format!(
                            "credential refresh failed; presenting the existing token: {e}"
                        ));
                        Some(t)
                    } else if p.presenting_certificate() {
                        self.log.warn(&format!(
                            "credential refresh failed; presenting the existing certificate: {e}"
                        ));
                        None
                    } else if let Some(refusal) = refusal.as_ref().filter(|r| r.held_user) {
                        self.log.warn(&format!(
                            "credential refused for its user and none is held; not sending: {e}"
                        ));
                        return Err(refusal.message.clone());
                    } else {
                        self.log.warn(&format!(
                            "token provider returned no token; sending unauthenticated: {e}"
                        ));
                        None
                    };
                    Ok((bearer, refusal.map(|r| r.message)))
                }
            }
        } else if !self.static_token.is_empty() {
            Ok((Some(self.static_token.clone()), None))
        } else {
            Ok((None, None))
        }
    }

    /// Test hook: shrink the reconnect backoff so reconnect tests run fast.
    #[cfg(test)]
    fn with_test_backoff(mut self, initial: Duration, max: Duration) -> Self {
        self.initial_backoff = initial;
        self.max_backoff = max;
        self
    }

    /// Test hook: shrink a respond's per-POST wait and whole budget.
    #[cfg(test)]
    fn with_test_respond_timing(mut self, attempt: Duration, budget: Duration) -> Self {
        self.respond_attempt_timeout = attempt;
        self.respond_budget = budget;
        self
    }
}

/// An inbound request as the subscribe loop hands it on: with the yield epoch of the
/// stream it came on ([`BusClient::with_presence`]).
pub(crate) struct Received {
    env: Envelope,
    epoch: u64,
}

/// The inbound side of a [`Subscription`].
pub struct EnvelopeRx {
    rx: mpsc::Receiver<Received>,
}

impl EnvelopeRx {
    /// The next inbound envelope; `None` once the subscription has ended.
    pub async fn recv(&mut self) -> Option<Envelope> {
        self.rx.recv().await.map(|r| r.env)
    }
}

/// A live namespace subscription: `rx` yields inbound envelopes; `status()` reports
/// the connection state. Dropping it aborts the loop.
pub struct Subscription {
    pub rx: EnvelopeRx,
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

/// Resolve when `wake` has been bumped since this receiver last looked. A dropped sender
/// (nothing can bump it any more) never resolves, so the select it sits in cannot spin.
async fn woken(wake: &mut watch::Receiver<u64>) {
    if wake.changed().await.is_err() {
        std::future::pending::<()>().await;
    }
}

/// The per-call state of one [`BusClient::respond`].
#[derive(Default)]
struct RespondCtx {
    /// The yield epoch this answer belongs to ([`BusClient::with_presence`]): once the
    /// watcher's has moved past it, nothing more is sent.
    epoch: u64,
    /// A POST of this answer went out and what it came to was not the shed server's own
    /// refusal (no answer in time, a failure after the request may have been written, a
    /// refusal that is not the API's own body): it may have reached the shed. Never
    /// cleared within a respond.
    maybe_delivered: bool,
    /// The respond's budget ([`RESPOND_RETRY_BUDGET`]) from the moment the first POST is
    /// sent; `None` until then. Every later step — a refusal body read, a retry wait, a credential fetch, a
    /// send — must finish by it; past it the respond stops (and is audited).
    deadline: Option<tokio::time::Instant>,
    /// The one invalidation (a 401 or a TLS alert) has been spent.
    reminted: bool,
    /// The one credential fetch that could mint has been made.
    mint_spent: bool,
    /// A POST went out and got no answer in time: the answer may or may not have
    /// reached the shed.
    unanswered: bool,
    /// The last send was not made, and why.
    not_sent: Option<NotSent>,
    /// The server's last refusal of this answer in this respond (a 401, or a 503/429
    /// before a retry), cleared when another POST goes out: what the audit says when the
    /// next send cannot be made.
    last_refusal: Option<SeenRefusal>,
}

impl RespondCtx {
    /// The server answered the last POST `status` with `code` (`None` unless the body was
    /// its own, read whole): remembered for the audit, and, unless it is the server's own
    /// refusal, the POST may have been delivered.
    fn saw_refusal(&mut self, status: u16, code: Option<String>) {
        if code.is_none() {
            self.maybe_delivered = true;
        }
        self.last_refusal = Some(SeenRefusal { status, code });
    }
}

/// Why a respond's send was not made ([`BusClient::send_respond`]).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum NotSent {
    /// The desktop app's embedded broker took over the request type on this machine
    /// (it runs now, or started since the handler did).
    LeftForApp,
    /// Less than a whole attempt was left of the respond's deadline.
    NoRoom,
}

/// A refusal of the answer seen earlier in a respond.
#[derive(Debug, Clone)]
struct SeenRefusal {
    status: u16,
    /// The `error.code`, when the body was the server's own (see
    /// [`RespondFailure::server_code`]).
    code: Option<String>,
}

/// Why a respond failed, with what the audit needs about it.
struct RespondFailure {
    err: BusError,
    /// The `error.code` of a refusal that is the shed server's OWN — a complete body in
    /// the API's error shape. `None` for a transport failure and for a status whose body
    /// is anything else (a proxy's 502, an empty or cut-short body): such a status says
    /// nothing certain about whether the answer reached the shed.
    server_code: Option<String>,
    /// The last POST got no answer within its wait: delivery is unknown.
    unanswered: bool,
    /// The respond ended on a send that was not made ([`NotSent`]).
    not_sent: Option<NotSent>,
    /// With `not_sent`: the server's last refusal of this answer before it, if any.
    last_refusal: Option<SeenRefusal>,
    /// An earlier POST of this answer may have reached the shed ([`RespondCtx`]).
    maybe_delivered: bool,
}

impl From<BusError> for RespondFailure {
    fn from(err: BusError) -> Self {
        RespondFailure {
            err,
            server_code: None,
            unanswered: false,
            not_sent: None,
            last_refusal: None,
            maybe_delivered: false,
        }
    }
}

/// Run `f` to completion, or until `deadline` passes (`None` when it did). No deadline
/// means no bound.
async fn until<F: std::future::Future>(
    deadline: Option<tokio::time::Instant>,
    f: F,
) -> Option<F::Output> {
    match deadline {
        None => Some(f.await),
        Some(d) => tokio::time::timeout_at(d, f).await.ok(),
    }
}

/// Whether, and after how long, a respond answered `status` is sent again: only a 503 or
/// 429 that carries `Retry-After` (the server's word that it wrote nothing), at most
/// [`RESPOND_MAX_RETRIES`] times. The value must be a non-negative whole number of
/// seconds (floored at [`RESPOND_RETRY_MIN_WAIT`]) whose wait, followed by one whole
/// `attempt` (the per-POST wait, [`RESPOND_ATTEMPT_TIMEOUT`]: the server's delivery
/// deadline plus the margin), ends inside what is left of the deadline (`remaining`).
/// Anything else — an HTTP date, a negative, a fraction, garbage, a list, a number too
/// large to hold, a wait past the deadline, a retry with no room for a whole attempt —
/// means do not retry.
fn respond_retry_wait(
    status: u16,
    retry_after: Option<&reqwest::header::HeaderValue>,
    retries_done: u32,
    remaining: Duration,
    attempt: Duration,
) -> Option<Duration> {
    if !matches!(status, 429 | 503) || retries_done >= RESPOND_MAX_RETRIES {
        return None;
    }
    let raw = retry_after?.to_str().ok()?.trim();
    if raw.is_empty() || !raw.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    let secs: u64 = raw.parse().ok()?;
    let wait = Duration::from_secs(secs).max(RESPOND_RETRY_MIN_WAIT);
    // The wait, then a whole attempt, inside what is left (and the wait itself strictly
    // inside it, whatever `attempt` is).
    (wait < remaining && wait.saturating_add(attempt) <= remaining).then_some(wait)
}

/// A non-2xx response body as read.
struct ErrorBody {
    /// At most [`MAX_ERROR_BODY_BYTES`], lossy-decoded and trimmed. RAW server text: for
    /// classification only; [`Self::shown`] is what may be shown or logged.
    raw: String,
    /// Read to its end inside the byte and time bounds. A body that was cut short — over
    /// the byte cap, slower than the time bound or the deadline, a read error, shutdown —
    /// is never classified as a pin refusal, nor as the server's own refusal, even when
    /// the part read parses on its own.
    complete: bool,
}

impl ErrorBody {
    /// The body as a person may see it (status, log lines): cleaned and capped.
    fn shown(&self) -> String {
        display_body(&self.raw)
    }

    /// The `error.code` of a COMPLETE body in the API's error shape — what marks a
    /// refusal as the shed server's own.
    fn server_code(&self) -> Option<String> {
        if !self.complete {
            return None;
        }
        parse_api_error(&self.raw)
            .and_then(|(code, _)| code)
            .filter(|c| !c.is_empty())
    }
}

/// Read at most [`MAX_ERROR_BODY_BYTES`] of a (non-2xx) response body, within
/// [`ERROR_BODY_READ_TIMEOUT`] (and by `deadline` when one is given), racing each read
/// against shutdown. Mirrors Go's `io.LimitReader(resp.Body, 1024)`: a hostile / huge
/// error body can't grow memory unbounded, and a stalled or never-ending body can't pin
/// the caller's loop or the daemon's teardown. Whatever stops the read early, what was
/// read so far is returned, marked incomplete.
async fn read_error_body(
    mut resp: reqwest::Response,
    shutdown: &watch::Receiver<bool>,
    deadline: Option<tokio::time::Instant>,
) -> ErrorBody {
    let bound = tokio::time::Instant::now() + ERROR_BODY_READ_TIMEOUT;
    let bound = deadline.map_or(bound, |d| d.min(bound));
    let mut buf: Vec<u8> = Vec::new();
    let complete = loop {
        let chunk = tokio::select! {
            _ = wait_shutdown(shutdown.clone()) => break false,
            _ = tokio::time::sleep_until(bound) => break false,
            c = resp.chunk() => c,
        };
        match chunk {
            Ok(Some(bytes)) => {
                if buf.len() + bytes.len() > MAX_ERROR_BODY_BYTES {
                    let take = MAX_ERROR_BODY_BYTES - buf.len();
                    buf.extend_from_slice(&bytes[..take]);
                    break false;
                }
                buf.extend_from_slice(&bytes);
            }
            Ok(None) => break true,
            Err(_) => break false,
        }
    };
    ErrorBody {
        raw: String::from_utf8_lossy(&buf).trim().to_string(),
        complete,
    }
}

/// Whether the bus follows a redirect to `next`, `hops` redirects into a chain: only to
/// the configured server's own origin (scheme, host and port), only over HTTPS (never a
/// plaintext downgrade), and at most [`MAX_REDIRECTS`] times. Every request the bus makes
/// carries this machine's identity headers and may present a client certificate, so
/// neither must ever reach another host — a redirect it does not follow is the answer.
fn redirect_allowed(configured: Option<&reqwest::Url>, next: &reqwest::Url, hops: usize) -> bool {
    next.scheme() == "https"
        && hops <= MAX_REDIRECTS
        && configured.is_some_and(|c| c.origin() == next.origin())
}

/// Build the reqwest client. Redirects stay inside the configured server's HTTPS
/// origin ([`redirect_allowed`]; fail-closed on a plaintext redirect, as shed-core's
/// `build_http_client`); with a `pin` it installs the
/// leaf-cert verifier. No global timeout — the SSE subscribe holds a long-lived
/// connection, and shutdown/`ctx` cancellation bound it instead.
/// A `client_certs` resolver, when the caller has a credential source that can produce a
/// client identity, installs the ADAPTIVE-TRANSPORT seam (plan 001 D5): the resolver hands
/// back the source's current certificate in mtls state and `None` in token state, both
/// read at HANDSHAKE time. The client is therefore built exactly once — a rotation, and
/// even a server-side token↔mtls flip, is a write into shared state rather than a rebuilt
/// `reqwest::Client` and a churned connection pool.
///
/// A caller with no source at all (an open server, or a static configured token) passes
/// `None` and gets the historical no-client-auth config. That is not just a shortcut: the
/// client-auth path also DISABLES TLS session resumption (see `shed_core::tls`, where the
/// reason is recorded), and there is no point paying that extra round trip on a transport
/// that could never present a certificate.
fn build_http_client(
    server_url: &str,
    pin: Option<&str>,
    client_certs: Option<Arc<ClientCertResolver>>,
) -> Result<reqwest::Client, BusError> {
    // An unparseable URL follows no redirect at all.
    let configured = reqwest::Url::parse(server_url).ok();
    let mut builder =
        reqwest::Client::builder().redirect(reqwest::redirect::Policy::custom(move |attempt| {
            if redirect_allowed(configured.as_ref(), attempt.url(), attempt.previous().len()) {
                attempt.follow()
            } else {
                attempt.stop()
            }
        }));
    if let Some(pin) = pin {
        let cfg = match client_certs {
            Some(resolver) => pinned_client_config_with_client_auth(pin, resolver),
            None => pinned_client_config(pin),
        }
        .map_err(|e| BusError::Config(e.to_string()))?;
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
    /// What this machine signs for a shed (`ssh.allow_users` + `ssh.offer_keys`, plan 035
    /// §3.5), checked before the approval gate. Fixed at start.
    pub sign_policy: Arc<SignPolicy>,
    /// Bounds the refusal audit lines one shed can cause ([`RefusalAuditLimiter`]).
    pub refusal_audits: Arc<RefusalAuditLimiter>,
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
    /// Bump to cut every waiting backoff in this group short ([`BusClient::retry_now`]).
    pub wake: Arc<watch::Sender<u64>>,
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

    // TLS leaf pin from the target fingerprint ALONE — never the token choice.
    // Computed ONCE here (not per-branch) so it can't be dropped on one path: Go's
    // `startWatcherGroup` passes `WithTLSPin(t.TLSFingerprint)` unconditionally
    // (supervisor.go). A discovery target can be https + pinned + static-token
    // (fingerprint set, no ssh endpoint ⇒ not minting); dropping the pin there — as
    // this code previously did on the static branch — sent the static bearer under
    // plain WebPKI against a pinned server. `BusClient::new` filters an empty
    // fingerprint (open http servers) to no-pin, and refuses a real pin on a
    // non-https URL (covered by `pin_on_*`/`empty_pin_is_ignored_on_http`).
    let pin = Some(target.tls_fingerprint.clone());

    // Provider from should_mint (Go's `startWatcherGroup`): a SECURE server
    // (https + ssh endpoint + minter) authenticates with a self-minted, auto-refreshing
    // credentials-scope token bridged onto the bus `TokenProvider`; an OPEN server
    // sends its (usually empty) static token.
    // The bus credential source doubles as this group's client-IDENTITY source: in mtls
    // mode it holds the certificate, and `cert_resolver()` is the handle the transport
    // reads at handshake time. An open/static-token server has neither, so both stay None
    // and the transport is byte-identical to what it was before mtls existed.
    let secure = crate::supervisor::should_mint(deps, target);
    // The one static bearer a group that does not mint may send — the bus provider's and
    // the egress stream's alike. Never a stored token for an entry with an identity file.
    let static_bearer = static_bearer(target);
    if !secure && static_bearer.is_empty() && !target.token.is_empty() {
        log.warn(&format!(
            "server {}: its credentials_token is not sent: the entry names an identity_file, \
             and a stored token does not say which SSH key it was issued for",
            shed_core::token::display_server_name(&target.name)
        ));
    }
    let mut refresh_source: Option<Arc<crate::minter::CredentialSource>> = None;
    let provider: Arc<dyn TokenProvider> = if secure {
        let minter = deps
            .minter
            .clone()
            .expect("should_mint implies a minter is present");
        // Fenced by this group's cancel: once the supervisor stops or replaces the group
        // (a changed `user` in config among the reasons), a mint it started that ends
        // late never writes over what the replacement stored (plan 037 §3.2.2).
        let src = crate::minter::new_group_credential_source(
            minter,
            target.clone(),
            crate::minter::SCOPE_CREDENTIALS,
            cancel_rx.clone(),
        );
        // A stored certificate the held-user rule refused at start (plan 037 §3.2.2):
        // say why the source mints instead of presenting it.
        if let Some(note) = src.hydration_note() {
            log.warn(note);
        }
        refresh_source = Some(src.clone());
        // `Arc<CredentialSource>: TokenProvider` (the bridge in `supervisor.rs`) — box it
        // into the trait object the client holds.
        Arc::new(src) as Arc<dyn TokenProvider>
    } else {
        Arc::new(StaticTokenProvider::new(static_bearer.clone())) as Arc<dyn TokenProvider>
    };

    let bus_certs = refresh_source.as_ref().map(|s| s.cert_resolver());
    let client = match BusClient::new(
        target.url.clone(),
        String::new(),
        Some(provider),
        pin,
        bus_certs,
        log.clone(),
    ) {
        Ok(c) => c
            .with_server_name(target.name.clone())
            .with_machine(deps.machine.clone())
            .with_presence(deps.embedded_presence.clone())
            .with_presence_lock(deps.presence_lock.clone()),
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
                wake: Arc::new(watch::channel(0u64).0),
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
        sign_policy: deps.ssh_sign_policy.clone(),
        // One per group: the windows are per shed of this server.
        refusal_audits: Arc::new(RefusalAuditLimiter::default()),
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

    // The refusal-summary writer: a group task, so on cancel it writes every pending
    // summary before `done` resolves, and no timer outlives the group.
    tasks.push(tokio::spawn(run_refusal_flusher(
        handlers.refusal_audits.clone(),
        handlers.audit.clone(),
        handlers.server_name.clone(),
        cancel_rx.clone(),
    )));

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
    // In mtls mode that own source is also the egress transport's own IDENTITY: one
    // certificate carries one scope, so the control-scoped stream must present the
    // control-scoped certificate, not the bus's credentials-scoped one.
    let mut egress_certs: Option<Arc<ClientCertResolver>> = None;
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
        egress_certs = Some(control.cert_resolver());
        Some(Arc::new(control) as Arc<dyn crate::egress::EgressTokenSource>)
    } else {
        None
    };
    let egress = crate::egress::EgressSubscriber::new(
        target.name.clone(),
        target.url.clone(),
        static_bearer,
        &target.tls_fingerprint,
        crate::egress::EgressCredentials {
            tokens: egress_tokens,
            client_certs: egress_certs,
        },
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
        wake: client.wake_handle(),
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
    // A request taken off the channel while a handler ran (the drain below) that is still
    // current: dealt with next, ahead of the channel.
    let mut next: Option<Received> = None;
    // The yield epoch whose queue has been drained.
    let mut drained = client.yield_epoch();
    loop {
        let got = match next.take() {
            Some(got) => got,
            None => tokio::select! {
                _ = wait_shutdown(shutdown.clone()) => break,
                e = sub.rx.rx.recv() => match e {
                    Some(got) => got,
                    None => break, // sender dropped: shutdown or terminal 409
                },
            },
        };
        // The desktop app's embedded broker took over this request type on this machine
        // (the daemon's watcher says so), or did since the request was received (its yield
        // epoch has been passed, even if the app has quit again since): the request is
        // left to whoever answered after that yield, unanswered here. It stays pending on
        // the server until it is answered: the registry hands a listener its user's
        // un-answered requests for the namespace that were delivered to the SAME machine
        // at the SAME binding generation (`internal/plugin/registry.go`,
        // `pendingForLocked`), and the app presents this machine's id (the two share
        // `machine.json`). If the app quit without answering it, this daemon's next
        // stream (opened under the new epoch) is that listener, and gets it again,
        // stamped current: nothing is lost.
        if client.embedded_present() || client.yielded_since(got.epoch) {
            leave_to_app(&client, namespace, &got.env, &log);
            continue;
        }
        // The handler answers through a copy that knows its epoch. While it runs, a start
        // of the app drains what is queued at that moment (each left as above), so no
        // request received before the yield waits behind the handler to be looked at.
        let handler_client = client.for_handler(got.epoch);
        let dispatch =
            dispatch_bus_message(&handler_client, namespace, &got.env, &shutdown, &handlers);
        tokio::pin!(dispatch);
        loop {
            tokio::select! {
                biased;
                _ = &mut dispatch => break,
                epoch = client.next_yield(drained), if next.is_none() => {
                    drained = epoch;
                    while next.is_none() {
                        match sub.rx.rx.try_recv() {
                            Ok(r) if client.yielded_since(r.epoch) || client.embedded_present() => {
                                leave_to_app(&client, namespace, &r.env, &log)
                            }
                            // Received after the yield (a newer stream): kept, in order.
                            Ok(r) => next = Some(r),
                            Err(_) => break,
                        }
                    }
                }
            }
        }
    }
    let s = sub.status();
    log.info(&format!(
        "message bus stopped namespace={} state={:?} last_error={}",
        s.namespace, s.state, s.last_error
    ));
}

/// Say that `env` is left to the desktop app's embedded broker (not handled here).
fn leave_to_app(client: &BusClient, namespace: &str, env: &Envelope, log: &Arc<dyn BusLog>) {
    log.info(&format!(
        "leaving request_id={} namespace={namespace} server={} to the desktop app's \
         embedded broker on this machine (it took over this request type; the server \
         delivers the request again to the next stream of this machine)",
        env.id,
        client.server_label()
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
/// dispatch. `ping` → `{"status":"ok"}`; `sign` → the checked, gated flow
/// ([`handle_sign`]: signing check → approval gate → backend → response + audit, a
/// refusal's audit line passed through the per-shed [`RefusalAuditLimiter`]); `list` →
/// the ungated key listing + a durable non-gated audit; `status` → `{connected, mode, key_count}` (no audit). An unknown
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
                    &handlers.sign_policy,
                    &handlers.gate,
                    &handlers.backend,
                )
                .await;
                (payload, admit_sign_audit(handlers, entry), "sign response")
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
    // later docker) namespaces (`respond_and_audit`). `ping`/`status`, an unknown op
    // and an undecodable payload leave `audit_entry` None (Go emits no audit for those);
    // `sign` audits every outcome, a refusal included, unless the limiter counted it.
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
    let delivered = client.respond_detailed(namespace, &resp, shutdown).await;
    match &delivered {
        Err(f) if f.not_sent == Some(NotSent::LeftForApp) => client.log.info(&format!(
            "not sending {what} request_id={} namespace={namespace} server={}: the desktop \
             app's embedded broker on this machine took over this request type, and the \
             server re-delivers the request to it",
            env.id,
            client.server_label(),
        )),
        Err(f) => client.log.warn(&format!(
            "failed to send {what} namespace={namespace} server={}: {}",
            client.server_label(),
            f.err
        )),
        Ok(()) => {}
    }
    // Audit after responding — the durable log + desktop fan-out. An answer the shed
    // server ITSELF refused (its own error body, read whole) never reached the shed, so
    // the entry says that rather than recording a credential handed over: e.g. a
    // signature made on a machine that stopped being the source while the person was
    // approving it. Any other status (a proxy's 502 can follow an answer that WAS
    // delivered) keeps the operation's result, the status noted as delivery unknown; so
    // does a POST that got no answer within its wait. Any other transport failure is
    // left as is: the answer may well have been delivered.
    //
    // An answer not sent because the desktop app's embedded broker took over is
    // `undelivered` when no POST of it was made, or every one made was refused by the
    // server itself (this answer never reached the shed), with its own reason: the app
    // answers the re-delivered request. When an earlier POST of it may have reached the
    // shed (no answer in time, a failure after the request may have been written, a
    // refusal that is not the server's own), only the retry was left to the app: the
    // result is kept and delivery is unknown. One that could not be sent again in time is
    // recorded by the last thing known about it: the server's earlier refusal (as that
    // refusal would be), or, with none, delivery unknown (an earlier POST may have been
    // delivered).
    if let Some(mut entry) = audit_entry {
        match &delivered {
            Err(RespondFailure {
                not_sent: Some(NotSent::LeftForApp),
                maybe_delivered: true,
                last_refusal,
                ..
            }) => {
                let earlier = match last_refusal {
                    Some(r) => format!("was answered with status {}", r.status),
                    None => "got no definite answer".to_string(),
                };
                append_reason(
                    &mut entry,
                    format!(
                        "an earlier send of the response {earlier}; delivery unknown; the \
                         retry was not sent: {AUDIT_NOTE_LEFT_FOR_APP} (its embedded broker on \
                         this machine took over this request type; the server re-delivers the \
                         request to it if it is still pending)"
                    ),
                );
            }
            Err(RespondFailure {
                not_sent: Some(NotSent::LeftForApp),
                ..
            }) => {
                let was =
                    std::mem::replace(&mut entry.result, AUDIT_RESULT_UNDELIVERED.to_string());
                append_reason(
                    &mut entry,
                    format!(
                        "result {was}, but not sent: {AUDIT_NOTE_LEFT_FOR_APP} (its embedded \
                         broker on this machine took over this request type; the server \
                         re-delivers the request to it)"
                    ),
                );
            }
            Err(RespondFailure {
                err: BusError::BadStatus(status, _),
                server_code,
                ..
            }) => mark_refused(&mut entry, *status, server_code.as_deref()),
            Err(RespondFailure {
                not_sent: Some(NotSent::NoRoom),
                last_refusal,
                ..
            }) => match last_refusal {
                Some(r) => mark_refused(&mut entry, r.status, r.code.as_deref()),
                None => append_reason(
                    &mut entry,
                    "the response could not be sent again within its deadline; delivery \
                     unknown"
                        .to_string(),
                ),
            },
            Err(RespondFailure {
                unanswered: true, ..
            }) => append_reason(
                &mut entry,
                format!(
                    "the response got no answer within {:?}; delivery unknown",
                    client.respond_attempt_timeout
                ),
            ),
            _ => {}
        }
        audit.log(entry);
    }
}

/// Append `note` to `entry.reason`, after any reason it already has.
fn append_reason(entry: &mut AuditEntry, note: String) {
    entry.reason = if entry.reason.is_empty() {
        note
    } else {
        format!("{}; {note}", entry.reason)
    };
}

/// Record on `entry` that the answer was refused with `status`. When the refusal is the
/// shed server's own (`server_code`), `result` becomes [`AUDIT_RESULT_UNDELIVERED`] and
/// `reason` keeps what the operation itself came to; otherwise `result` is kept and the
/// status is noted with "delivery unknown".
fn mark_refused(entry: &mut AuditEntry, status: u16, server_code: Option<&str>) {
    let note = match server_code {
        Some(code) => {
            let code: String = code
                .chars()
                .filter(|c| c.is_ascii_alphanumeric() || *c == '_')
                .take(MAX_CODE_CHARS)
                .collect();
            let was = std::mem::replace(&mut entry.result, AUDIT_RESULT_UNDELIVERED.to_string());
            format!("result {was}, but the server refused the response (status {status} {code})")
        }
        None => format!("the response was answered with status {status}; delivery unknown"),
    };
    append_reason(entry, note);
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
            let entry = list_audit(
                server_name,
                shed_name,
                "ok",
                &format!("{} keys", keys.len()),
            );
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

/// The audit entry for a sign request the signing check declined (plan 035 §3.5):
/// `result: "refused"`, the audit-only code [`AUDIT_CODE_SIGN_REFUSED`], `reason` the
/// refusal class's stable name, `detail` the refusal's own bounded, escaped detail (never
/// the request), `approval: "none"` (the gate was never asked), and no
/// decided_by/scope/ttl.
fn sign_refused_audit(server_name: &str, shed_name: &str, refusal: &Refusal) -> AuditEntry {
    AuditEntry {
        server: server_name.to_string(),
        shed: shed_name.to_string(),
        ns: NS_SSH_AGENT.to_string(),
        op: "sign".to_string(),
        result: AUDIT_RESULT_REFUSED.to_string(),
        detail: refusal.detail().to_string(),
        code: AUDIT_CODE_SIGN_REFUSED.to_string(),
        reason: refusal.class().as_str().to_string(),
        approval: "none".to_string(),
        ..Default::default()
    }
}

/// The refusals one shed's window counted instead of logging one by one, per class.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RefusalSummary {
    pub shed: String,
    /// The count per class, in [`RefusalClass::ALL`] order.
    pub counts: [u64; REFUSAL_CLASSES],
}

impl RefusalSummary {
    pub fn total(&self) -> u64 {
        self.counts.iter().fold(0u64, |a, n| a.saturating_add(*n))
    }

    /// `class=count` for each class counted, in [`RefusalClass::ALL`] order: at most
    /// seven short pairs, so the line stays bounded.
    pub fn by_class(&self) -> String {
        RefusalClass::ALL
            .iter()
            .zip(self.counts)
            .filter(|(_, n)| *n > 0)
            .map(|(class, n)| format!("{class}={n}"))
            .collect::<Vec<_>>()
            .join(" ")
    }
}

/// The time source and timer the refusal limiter runs on: the system clock in
/// production ([`SystemRefusalClock`]), a hand-driven one in tests, so that no window
/// test depends on how fast the machine is.
pub trait RefusalClock: Send + Sync {
    fn now(&self) -> Instant;
    /// Resolve once [`RefusalClock::now`] has reached `at`.
    fn sleep_until(
        &self,
        at: Instant,
    ) -> std::pin::Pin<Box<dyn std::future::Future<Output = ()> + Send + 'static>>;
}

/// The system clock and tokio's timer.
pub struct SystemRefusalClock;

impl RefusalClock for SystemRefusalClock {
    fn now(&self) -> Instant {
        Instant::now()
    }
    fn sleep_until(
        &self,
        at: Instant,
    ) -> std::pin::Pin<Box<dyn std::future::Future<Output = ()> + Send + 'static>> {
        Box::pin(tokio::time::sleep_until(tokio::time::Instant::from_std(at)))
    }
}

/// Bounds the audit lines refused sign requests can write, per shed.
///
/// Nothing else bounds them: shed-server does not limit how many bus requests a shed
/// sends, the broker answers a server's `ssh-agent` requests one at a time (so the only
/// limit is the round trip), and the audit file is never rotated. A refusal costs the
/// broker no prompt and no backend call, so a shed could otherwise grow the audit log
/// (and the desktop's activity feed) as fast as it can send.
///
/// Per shed, in each fixed [`REFUSAL_AUDIT_WINDOW`] (a window opens at the shed's first
/// refusal after the previous one ended):
/// - the first [`REFUSAL_AUDIT_BURST`] refusals get their own line;
/// - after that, the first refusal of each class not yet logged in the window still gets
///   its own line, so the username or namespace behind each kind of refusal is on record
///   once per class per window;
/// - every other refusal is counted under its class, and the counts are written as one
///   summary line ([`AUDIT_REASON_RATE_LIMITED`], each class with its count) once the
///   window has ended.
///
/// So every refused request is either logged or counted under its own class, and the
/// worst case for one shed in one window is `burst + 7 + 1` lines (the burst, one more
/// line per class, one summary). The summary is written by the group's flusher task
/// ([`run_refusal_flusher`]) at the window's end, or before the shed's next refusal if
/// that comes first; both take the counts under one lock, so they are written once.
///
/// At most [`REFUSAL_AUDIT_MAX_SHEDS`] windows are kept. A new shed past that cap first
/// drops the windows that have ended, then evicts the oldest open window; either way the
/// counts a dropped window held are written as its summary then, never lost. (An evicted
/// shed's next refusal opens a fresh window, so the per-window bound holds per window
/// kept; eviction needs more sheds refusing at once than the cap.)
///
/// At shutdown the flusher writes every pending summary before the group's `done`
/// resolves ([`RefusalAuditLimiter::close`]), and from then on every refusal is logged
/// one by one. What a hard kill (SIGKILL, a crash, power loss) still loses: the counts of
/// windows still open — at most one window's worth of counted refusals per shed — whose
/// summary had not been written yet. Every line already written is kept.
///
/// Only refusals are limited: ok, denied and error lines each follow a prompt or a
/// backend call, as before. The decisions are pure (the caller passes the time); the
/// clock is only used by the callers ([`RefusalAuditLimiter::now`],
/// [`run_refusal_flusher`]).
pub struct RefusalAuditLimiter {
    burst: u32,
    window: Duration,
    max_sheds: usize,
    clock: Arc<dyn RefusalClock>,
    /// Woken when a window opens, so the flusher learns of its deadline.
    opened: tokio::sync::Notify,
    state: Mutex<LimiterState>,
}

#[derive(Default)]
struct LimiterState {
    sheds: HashMap<String, RefusalWindow>,
    /// Orders windows that opened at the same instant, for eviction.
    next_seq: u64,
    /// Set by [`RefusalAuditLimiter::close`]: every refusal is then logged.
    closed: bool,
}

struct RefusalWindow {
    start: Instant,
    seq: u64,
    /// Lines written from the burst.
    logged: u32,
    /// One bit per class (in [`RefusalClass::ALL`] order) that has had a line.
    classes_logged: u8,
    /// Refusals counted instead of logged, per class.
    counted: [u64; REFUSAL_CLASSES],
}

impl RefusalWindow {
    fn open(start: Instant, seq: u64) -> RefusalWindow {
        RefusalWindow {
            start,
            seq,
            logged: 0,
            classes_logged: 0,
            counted: [0; REFUSAL_CLASSES],
        }
    }

    fn summary(&self, shed: &str) -> Option<RefusalSummary> {
        self.counted.iter().any(|n| *n > 0).then(|| RefusalSummary {
            shed: shed.to_string(),
            counts: self.counted,
        })
    }
}

/// What to do with one refusal's audit line.
#[derive(Debug, PartialEq, Eq)]
pub struct RefusalAdmission {
    /// Write the refusal's own line (otherwise it was counted).
    pub log: bool,
    /// Summaries to write first: this shed's ended window, or windows dropped to make
    /// room for this shed.
    pub summaries: Vec<RefusalSummary>,
}

fn class_index(class: RefusalClass) -> usize {
    RefusalClass::ALL
        .iter()
        .position(|c| *c == class)
        .expect("RefusalClass::ALL lists every class")
}

impl Default for RefusalAuditLimiter {
    fn default() -> Self {
        RefusalAuditLimiter::new(REFUSAL_AUDIT_BURST, REFUSAL_AUDIT_WINDOW)
    }
}

impl RefusalAuditLimiter {
    pub fn new(burst: u32, window: Duration) -> Self {
        RefusalAuditLimiter::with_clock(
            burst,
            window,
            REFUSAL_AUDIT_MAX_SHEDS,
            Arc::new(SystemRefusalClock),
        )
    }

    /// A limiter on `clock`, keeping at most `max_sheds` (at least one) windows.
    pub fn with_clock(
        burst: u32,
        window: Duration,
        max_sheds: usize,
        clock: Arc<dyn RefusalClock>,
    ) -> Self {
        RefusalAuditLimiter {
            burst,
            window,
            max_sheds: max_sheds.max(1),
            clock,
            opened: tokio::sync::Notify::new(),
            state: Mutex::new(LimiterState::default()),
        }
    }

    /// The limiter's clock's time.
    pub fn now(&self) -> Instant {
        self.clock.now()
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, LimiterState> {
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Decide about one refusal of `class` from `shed` at `now`.
    pub fn admit(&self, shed: &str, class: RefusalClass, now: Instant) -> RefusalAdmission {
        let mut summaries = Vec::new();
        let mut opened = false;
        let log = {
            let mut st = self.lock();
            if st.closed {
                return RefusalAdmission {
                    log: true,
                    summaries,
                };
            }
            if !st.sheds.contains_key(shed) {
                self.make_room(&mut st, now, &mut summaries);
                let seq = st.next_seq;
                st.next_seq = st.next_seq.wrapping_add(1);
                st.sheds
                    .insert(shed.to_string(), RefusalWindow::open(now, seq));
                opened = true;
            }
            let window = self.window;
            let LimiterState {
                sheds, next_seq, ..
            } = &mut *st;
            let w = sheds.get_mut(shed).expect("inserted above");
            if now.duration_since(w.start) >= window {
                summaries.extend(w.summary(shed));
                *w = RefusalWindow::open(now, *next_seq);
                *next_seq = next_seq.wrapping_add(1);
                opened = true;
            }
            let i = class_index(class);
            let bit = 1u8 << i;
            if w.logged < self.burst {
                w.logged += 1;
                w.classes_logged |= bit;
                true
            } else if w.classes_logged & bit == 0 {
                w.classes_logged |= bit;
                true
            } else {
                w.counted[i] = w.counted[i].saturating_add(1);
                false
            }
        };
        if opened {
            self.opened.notify_one();
        }
        RefusalAdmission { log, summaries }
    }

    /// Make room for one more window: drop the windows that have ended, then, while
    /// still at the cap, the oldest open one; the counts of each go to `out`.
    fn make_room(&self, st: &mut LimiterState, now: Instant, out: &mut Vec<RefusalSummary>) {
        if st.sheds.len() < self.max_sheds {
            return;
        }
        let window = self.window;
        st.sheds.retain(|shed, w| {
            if now.duration_since(w.start) < window {
                return true;
            }
            out.extend(w.summary(shed));
            false
        });
        while st.sheds.len() >= self.max_sheds {
            let Some(oldest) = st
                .sheds
                .iter()
                .min_by_key(|(_, w)| (w.start, w.seq))
                .map(|(shed, _)| shed.clone())
            else {
                break;
            };
            if let Some(w) = st.sheds.remove(&oldest) {
                out.extend(w.summary(&oldest));
            }
        }
    }

    /// Take every window that has ended by `now`: the counts of those that counted any,
    /// to be written once, and the windows themselves dropped.
    pub fn flush_due(&self, now: Instant) -> Vec<RefusalSummary> {
        let mut out = Vec::new();
        let window = self.window;
        self.lock().sheds.retain(|shed, w| {
            if now.duration_since(w.start) < window {
                return true;
            }
            out.extend(w.summary(shed));
            false
        });
        out
    }

    /// When the earliest window still kept ends.
    pub fn next_deadline(&self) -> Option<Instant> {
        self.lock()
            .sheds
            .values()
            .map(|w| w.start + self.window)
            .min()
    }

    /// Take every window's counts, ended or not, and stop counting: from now on every
    /// refusal is logged one by one. Called once, at shutdown.
    pub fn close(&self) -> Vec<RefusalSummary> {
        let mut st = self.lock();
        st.closed = true;
        st.sheds
            .drain()
            .filter_map(|(shed, w)| w.summary(&shed))
            .collect()
    }

    /// The summary line for `summary`, written for this limiter's limits.
    pub fn summary_audit(&self, server_name: &str, summary: &RefusalSummary) -> AuditEntry {
        AuditEntry {
            server: server_name.to_string(),
            shed: summary.shed.clone(),
            ns: NS_SSH_AGENT.to_string(),
            op: "sign".to_string(),
            result: AUDIT_RESULT_REFUSED.to_string(),
            detail: format!(
                "{} more refused sign requests were not logged one by one \
                 (at most {} per {}s per shed, plus the first of each class): {}",
                summary.total(),
                self.burst,
                self.window.as_secs(),
                summary.by_class()
            ),
            code: AUDIT_CODE_SIGN_REFUSED.to_string(),
            reason: AUDIT_REASON_RATE_LIMITED.to_string(),
            approval: "none".to_string(),
            ..Default::default()
        }
    }

    #[cfg(test)]
    fn windows(&self) -> usize {
        self.lock().sheds.len()
    }
}

/// One server group's refusal-summary writer: sleeps until the earliest window ends
/// (woken early when a window opens), writes the summaries of the windows that ended,
/// and repeats until `shutdown`, when it writes every summary still pending
/// ([`RefusalAuditLimiter::close`]) before returning. It is one of the group's tasks, so
/// the group's `done` resolves only after that last write, and no timer outlives it.
pub async fn run_refusal_flusher(
    limiter: Arc<RefusalAuditLimiter>,
    audit: Arc<dyn AuditSink>,
    server_name: String,
    shutdown: watch::Receiver<bool>,
) {
    let stop = wait_shutdown(shutdown);
    tokio::pin!(stop);
    loop {
        for summary in limiter.flush_due(limiter.now()) {
            audit.log(limiter.summary_audit(&server_name, &summary));
        }
        let next = limiter.next_deadline();
        let timer = async {
            match next {
                Some(at) => limiter.clock.sleep_until(at).await,
                None => std::future::pending::<()>().await,
            }
        };
        tokio::select! {
            _ = &mut stop => break,
            _ = limiter.opened.notified() => {}
            _ = timer => {}
        }
    }
    for summary in limiter.close() {
        audit.log(limiter.summary_audit(&server_name, &summary));
    }
}

/// Pass a sign op's audit entry through the refusal limiter: anything but a refusal is
/// returned as is; a refusal is returned to be written or counted (`None`), any
/// summaries the admission produced written first.
fn admit_sign_audit(handlers: &BusHandlers, entry: AuditEntry) -> Option<AuditEntry> {
    if entry.result != AUDIT_RESULT_REFUSED {
        return Some(entry);
    }
    // The reason of a refusal line is its class's name (`sign_refused_audit`).
    let Some(class) = RefusalClass::ALL
        .iter()
        .copied()
        .find(|c| c.as_str() == entry.reason)
    else {
        return Some(entry);
    };
    let limiter = &handlers.refusal_audits;
    let admission = limiter.admit(&entry.shed, class, limiter.now());
    for summary in &admission.summaries {
        handlers
            .audit
            .log(limiter.summary_audit(&handlers.server_name, summary));
    }
    admission.log.then_some(entry)
}

/// What a shed is told when the signing check declines its request. The shed learns no
/// more than it could before this check existed: a key that does not even parse gets
/// the reply it always got (`invalid public key`, `KEY_NOT_FOUND`), and every other
/// refusal — whatever its class — the reply a failed signature always got (`sign
/// operation failed`, `SIGN_FAILED`), which is also what a key that is not offered got
/// from the key filter. The class is in the machine's audit log only.
fn sign_refusal_reply(refusal: &Refusal, public_key: &[u8]) -> serde_json::Value {
    if refusal.class() == RefusalClass::MalformedKey
        && ssh_key::public::PublicKey::from_bytes(public_key).is_err()
    {
        ssh_error("invalid public key", SSH_CODE_KEY_NOT_FOUND)
    } else {
        ssh_error("sign operation failed", SSH_CODE_SIGN_FAILED)
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

/// The gated `sign` flow. Returns the response payload (a `SSHSignResponse` on success,
/// else a `SSHErrorResponse`) plus the audit entry to write — every path audits.
///
/// ORDER (plan 035 §3.5): decode the request → decode the key and the data → **the
/// signing check** ([`SignPolicy::check`], with the request's flags) → approval gate →
/// sign through the check's [`crate::sign_check::Approved`], which hands the backend
/// exactly the buffers and flags that were checked. A request that fails to decode or
/// that the check declines never reaches the gate: no prompt, no approval cache read or
/// written, no backend call; it is audited `refused` with its class
/// ([`sign_refused_audit`]) and answered with [`sign_refusal_reply`] (or, for a request
/// that does not decode, the reply that step always gave). The ok, denied and error
/// audits are as before (detail = the key type).
async fn handle_sign(
    env: &Envelope,
    server_name: &str,
    shed_name: &str,
    policy: &SignPolicy,
    gate: &Arc<dyn ApprovalGate>,
    backend: &Arc<dyn SshBackend>,
) -> (serde_json::Value, AuditEntry) {
    let refused = |refusal: Refusal, reply: serde_json::Value| {
        (reply, sign_refused_audit(server_name, shed_name, &refusal))
    };

    // 1. Decode the sign request (a wrong-typed field → invalid sign request).
    let req: SignRequestPayload =
        match serde_json::from_value(env.payload.clone().unwrap_or_default()) {
            Ok(r) => r,
            Err(_) => {
                return refused(
                    Refusal::malformed_request("invalid sign request"),
                    ssh_error("invalid sign request", SSH_CODE_INTERNAL),
                )
            }
        };

    // 2. Decode the key and the data. These two buffers are the ones checked AND the
    //    ones signed: the check borrows them and signing goes through what it returns.
    let pub_bytes = match decode_b64_lenient(&req.public_key) {
        Ok(b) => b,
        Err(_) => {
            return refused(
                Refusal::malformed_request("invalid public key encoding"),
                ssh_error("invalid public key encoding", SSH_CODE_INTERNAL),
            )
        }
    };
    let data = match decode_b64_lenient(&req.data) {
        Ok(d) => d,
        Err(_) => {
            return refused(
                Refusal::malformed_request("invalid challenge data encoding"),
                ssh_error("invalid challenge data encoding", SSH_CODE_INTERNAL),
            )
        }
    };

    // 3. The signing check, BEFORE the gate: the key parses (exactly as `ssh_key` decodes
    //    it), is offered, and the data is a login as an allowed username or a `git`
    //    SSHSIG whose hash the flags agree with.
    let approved = match policy.check(&pub_bytes, &data, req.flags) {
        Ok(approved) => approved,
        Err(refusal) => {
            let reply = sign_refusal_reply(&refusal, &pub_bytes);
            return refused(refusal, reply);
        }
    };
    let key_type = approved.key_type();

    // 4. The approval gate (deny-all default fails closed). The reason shown to the app
    //    is the fixed "SSH sign request" (Go's desktopGate reason).
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
        return (ssh_error("approval denied", SSH_CODE_SIGN_FAILED), entry);
    }

    // 5. Sign the checked buffers under the checked flags — the only way to reach the
    //    backend's `sign`. Any backend error → {sign operation failed, SIGN_FAILED} +
    //    error audit (detail = key type). Success → SSHSignResponse + ok audit.
    match approved.sign(backend.as_ref()) {
        Ok(sig) => {
            let resp = SshSignResponse {
                format: sig.format,
                blob: base64::engine::general_purpose::STANDARD.encode(&sig.blob),
                rest: String::new(),
            };
            let payload = to_payload(&resp);
            let entry = sign_audit(server_name, shed_name, "ok", key_type, approval, &outcome);
            (payload, entry)
        }
        Err(_) => {
            let entry = sign_audit(
                server_name,
                shed_name,
                "error",
                key_type,
                approval,
                &outcome,
            );
            (
                ssh_error("sign operation failed", SSH_CODE_SIGN_FAILED),
                entry,
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
            DOCKER_OP_LIST => handle_docker_list(server_name, shed_name, &docker.backend).await,
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
            Err(_) => {
                return (
                    docker_error("invalid get request", DOCKER_CODE_INTERNAL),
                    None,
                )
            }
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
    /// reconnect logging can be asserted (mirrors Go's TestReconnectLogDedup), and
    /// keeps every line (`"LEVEL msg"`) so its text can be.
    #[derive(Default)]
    struct CountingLog {
        warns: AtomicUsize,
        debugs: AtomicUsize,
        errors: AtomicUsize,
        lines: Mutex<Vec<String>>,
    }
    impl CountingLog {
        fn keep(&self, level: &str, msg: &str) {
            self.lines.lock().unwrap().push(format!("{level} {msg}"));
        }
        fn lines(&self) -> Vec<String> {
            self.lines.lock().unwrap().clone()
        }
    }
    impl BusLog for CountingLog {
        fn info(&self, msg: &str) {
            self.keep("INFO", msg);
        }
        fn warn(&self, msg: &str) {
            self.warns.fetch_add(1, Ordering::SeqCst);
            self.keep("WARN", msg);
        }
        fn debug(&self, msg: &str) {
            self.debugs.fetch_add(1, Ordering::SeqCst);
            self.keep("DEBUG", msg);
        }
        fn error(&self, msg: &str) {
            self.errors.fetch_add(1, Ordering::SeqCst);
            self.keep("ERROR", msg);
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
        open_client_with(base_url, silent_log())
    }

    fn open_client_with(base_url: &str, log: Arc<dyn BusLog>) -> BusClient {
        BusClient::new(base_url.to_string(), String::new(), None, None, None, log).unwrap()
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
            sign_policy: Arc::new(SignPolicy::default()),
            refusal_audits: Arc::new(RefusalAuditLimiter::default()),
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
            None,
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
            None,
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
            None,
            silent_log(),
        );
        assert!(r.is_ok());
    }

    /// The client-auth seam is installed when — and only when — a credential source
    /// supplies a resolver, and the resulting rustls config presents whatever that
    /// resolver currently holds. Disabling session resumption on that path is asserted
    /// too: a resumed TLS 1.3 handshake sends NO Certificate message, so the server would
    /// carry a rotated-away identity forward (`shed_core::tls` records the finding).
    #[test]
    fn client_auth_is_installed_only_with_a_resolver() {
        use shed_core::tls::{
            pinned_client_config, pinned_client_config_with_client_auth, ClientCertResolver,
        };
        const PIN: &str = "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824";

        // No resolver → no client certificate can ever be presented.
        let plain = pinned_client_config(PIN).unwrap();
        assert!(plain.client_auth_cert_resolver.resolve(&[], &[]).is_none());

        // With a resolver → the config reads THROUGH it, live, at handshake time.
        let resolver = Arc::new(ClientCertResolver::new());
        let cfg = pinned_client_config_with_client_auth(PIN, resolver.clone()).unwrap();
        assert!(
            cfg.client_auth_cert_resolver.resolve(&[], &[]).is_none(),
            "token state presents nothing"
        );

        // Arming the source's resolver changes what the ALREADY-BUILT config presents —
        // no rebuild, which is the entire adaptive-transport claim. (No userId: this arms
        // the resolver directly, a layer below adoption's user rule.)
        let key = rcgen::KeyPair::generate().unwrap();
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params
            .distinguished_name
            .push(rcgen::DnType::CommonName, "SHA256:agent");
        let leaf = params.self_signed(&key).unwrap();
        let certified = shed_core::tls::certified_key_from_pem(
            &shed_core::csr::pem_encode(shed_core::csr::PEM_LABEL_CERTIFICATE, leaf.der()),
            &key.serialize_der(),
        )
        .unwrap();
        resolver.set(Some(Arc::new(certified)));
        assert!(
            cfg.client_auth_cert_resolver.resolve(&[], &[]).is_some(),
            "mtls state presents the current certificate"
        );
        // Resumption is off on the client-auth path (rustls exposes no predicate for it,
        // so the store's Debug rendering is the observable): a resumed TLS 1.3 handshake
        // sends no Certificate message, and the server would carry a rotated-away identity
        // forward — `shed_core::tls` records the finding.
        assert!(
            format!("{:?}", cfg.resumption).contains("NoClientSessionStorage"),
            "resumption must stay off on the client-auth path: {:?}",
            cfg.resumption
        );

        // Both shapes build a usable BusClient over https.
        for certs in [None, Some(resolver)] {
            let r = BusClient::new(
                "https://mini2:8443".to_string(),
                String::new(),
                None,
                Some(PIN.into()),
                certs,
                silent_log(),
            );
            assert!(r.is_ok());
        }
    }

    /// In mtls state the credential source resolves to an EMPTY bearer, so the bus sends
    /// no `Authorization` header at all — the certificate is the credential (plan 001 D2:
    /// the middleware ignores the header, so sending one is pure downside).
    #[tokio::test]
    async fn no_bearer_header_is_sent_in_mtls_state() {
        struct MtlsProvider;
        #[async_trait::async_trait]
        impl TokenProvider for MtlsProvider {
            async fn token(&self) -> Result<String, BusError> {
                Ok(String::new()) // what CredentialSource::token yields in mtls state
            }
            fn invalidate(&self) {}
        }
        let client = BusClient::new(
            "https://x:8443".to_string(),
            "a-static-token-that-must-not-leak".to_string(),
            Some(Arc::new(MtlsProvider)),
            None,
            None,
            silent_log(),
        )
        .unwrap();
        assert_eq!(
            client.bearer().await,
            None,
            "an mtls credential must not send a bearer, nor fall back to the static token"
        );
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

    /// Blocker-2 regression (whole-branch review): a TLS certificate alert on
    /// the respond POST — the shape an mtls-mode server produces, with no HTTP
    /// status at all — must be classified as an auth failure: one invalidate,
    /// one retry, and when the retry is also refused the error surfaces with
    /// the alert in it. Never a second mint or a third send.
    #[tokio::test]
    async fn respond_tls_auth_failure_invalidates_once_and_bounds_sends() {
        let srv =
            crate::testalert::AlertServer::spawn(crate::testalert::CERTIFICATE_REQUIRED).await;
        let tp = SeqTokenProvider::new();
        let client = BusClient::new(
            srv.base_url(),
            String::new(),
            Some(tp.clone()),
            None,
            None,
            silent_log(),
        )
        .unwrap();
        let env = Envelope::new_response("r", "ns", serde_json::Value::Null);
        let err = client
            .respond("ns", &env, &never_shutdown())
            .await
            .unwrap_err();
        assert!(
            matches!(&err, BusError::Transport(m) if m.contains("received fatal alert")),
            "the flattened alert must survive into the surfaced error: {err:?}"
        );
        assert_eq!(
            tp.invalidated(),
            1,
            "exactly one invalidate for the TLS-shaped rejection"
        );
        assert_eq!(srv.accepts(), 2, "initial send + one retry, never a third");
    }

    /// Without a provider there is nothing to re-mint: the classified failure
    /// surfaces immediately, no retry.
    #[tokio::test]
    async fn respond_tls_auth_failure_without_provider_does_not_retry() {
        let srv =
            crate::testalert::AlertServer::spawn(crate::testalert::CERTIFICATE_REQUIRED).await;
        let env = Envelope::new_response("r", "ns", serde_json::Value::Null);
        let err = open_client(&srv.base_url())
            .respond("ns", &env, &never_shutdown())
            .await
            .unwrap_err();
        assert!(matches!(&err, BusError::Transport(_)), "err = {err:?}");
        assert_eq!(srv.accepts(), 1, "no provider → no retry");
    }

    /// Blocker-2 regression, subscribe side: a TLS certificate alert at connect
    /// takes the Unauthorized arm of the reconnect loop, so the provider is
    /// invalidated and the backoff-reconnect re-mints — instead of replaying
    /// the refused credential forever as a plain Transport error.
    #[tokio::test]
    async fn subscribe_tls_auth_connect_failure_invalidates() {
        let srv =
            crate::testalert::AlertServer::spawn(crate::testalert::CERTIFICATE_REQUIRED).await;
        let tp = SeqTokenProvider::new();
        let client = BusClient::new(
            srv.base_url(),
            String::new(),
            Some(tp.clone()),
            None,
            None,
            silent_log(),
        )
        .unwrap()
        .with_test_backoff(Duration::from_millis(5), Duration::from_millis(20));
        let (tx, rx) = shutdown_pair();
        let _sub = client.subscribe("ssh-agent", rx);
        let deadline = Instant::now() + Duration::from_secs(5);
        while tp.invalidated() == 0 && Instant::now() < deadline {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        let _ = tx.send(true);
        assert!(
            tp.invalidated() >= 1,
            "the classified connect failure must invalidate the token"
        );
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
        let log = Arc::new(CountingLog::default());
        let client = open_client_with(&server.base_url(), log.clone()).with_server_name("mini2");
        let (_tx, rx) = shutdown_pair();
        let mut sub = client.subscribe("ns1", rx);
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
        // The one ERROR line names the namespace AND the server (#414: a log that
        // brokers several servers must say which one refused).
        let errors: Vec<String> = log
            .lines()
            .into_iter()
            .filter(|l| l.starts_with("ERROR "))
            .collect();
        assert_eq!(errors.len(), 1, "{errors:?}");
        assert!(
            errors[0].contains("SSE subscription rejected")
                && errors[0].contains("namespace ns1 server=mini2 "),
            "{errors:?}"
        );
    }

    /// The connect line names its server: the config name when the supervisor gave
    /// one, else (the unnamed single-server target) the base URL.
    #[tokio::test]
    async fn the_connect_line_names_its_server() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/plugins/listeners/ssh-agent/messages");
                t.status(200)
                    .header("content-type", "text/event-stream")
                    .body("data: {\"id\":\"e1\",\"namespace\":\"ssh-agent\",\"type\":\"request\",\"final\":true,\"timestamp\":\"t\",\"payload\":{}}\n\n");
            })
            .await;
        for (name, want) in [("mini2", "mini2".to_string()), ("", server.base_url())] {
            let log = Arc::new(CountingLog::default());
            let client = open_client_with(&server.base_url(), log.clone()).with_server_name(name);
            let (tx, rx) = shutdown_pair();
            let mut sub = client.subscribe("ssh-agent", rx);
            tokio::time::timeout(Duration::from_secs(5), sub.rx.recv())
                .await
                .expect("an envelope")
                .unwrap();
            let _ = tx.send(true);
            let want = format!("INFO SSE connected namespace=ssh-agent server={want}");
            assert!(log.lines().contains(&want), "{want:?} in {:?}", log.lines());
        }
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

    /// When the mint a request's own credential fetch made was refused for its
    /// certificate's user (plan 037 §3.2.2), the status's last error after the server's
    /// 401 leads with that refusal; without one it is the 401 alone.
    #[tokio::test]
    async fn a_held_user_refusal_leads_the_401_reason_in_the_status() {
        const REFUSAL: &str = "server \"s\": the server issued a certificate for user \"admin\", \
            but this entry is held to \"pat\"; it was not used.";
        struct RefusedProvider(Option<&'static str>);
        #[async_trait::async_trait]
        impl TokenProvider for RefusedProvider {
            async fn token(&self) -> Result<String, BusError> {
                unreachable!("a request fetches through token_for_request")
            }
            async fn token_for_request(&self) -> Result<String, (BusError, Option<MintRefusal>)> {
                Err((
                    BusError::Config(REFUSAL.into()),
                    self.0.map(|m| MintRefusal {
                        message: m.to_string(),
                        held_user: true,
                    }),
                ))
            }
            fn invalidate(&self) {}
            // A certificate for the held user is still armed, so the request is sent
            // and the refusal explains the server's answer to it.
            fn presenting_certificate(&self) -> bool {
                true
            }
        }
        for (refusal, starts) in [(Some(REFUSAL), REFUSAL), (None, "unexpected status 401")] {
            let server = MockServer::start_async().await;
            server
                .mock_async(|w, t| {
                    w.method(GET)
                        .path("/api/plugins/listeners/ssh-agent/messages");
                    t.status(401);
                })
                .await;
            let client = BusClient::new(
                server.base_url(),
                String::new(),
                Some(Arc::new(RefusedProvider(refusal))),
                None,
                None,
                silent_log(),
            )
            .unwrap()
            .with_test_backoff(Duration::from_millis(5), Duration::from_millis(20));
            let (_tx, rx) = shutdown_pair();
            let sub = client.subscribe("ssh-agent", rx);
            let mut last = String::new();
            for _ in 0..200 {
                last = sub.status().last_error;
                if !last.is_empty() {
                    break;
                }
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
            assert!(last.starts_with(starts), "{last}");
            assert!(last.contains("unexpected status 401"), "{last}");
            if refusal.is_some() {
                assert!(
                    last.contains("(request refused: unexpected status 401"),
                    "{last}"
                );
            }
        }
    }

    // ---- plan 037 A23: a held-user refusal with nothing to present sends nothing ----

    const HELD_USER_REFUSAL: &str = "server \"s\": the server issued a certificate for user \
        \"admin\", but this entry is held to \"pat\"; it was not used.";

    /// A provider whose every fetch fails with a mint refusal: the held-user rule's or
    /// another, with or without a certificate still armed.
    struct RefusingProvider {
        held_user: bool,
        presenting: bool,
        fetches: AtomicUsize,
    }
    impl RefusingProvider {
        fn new(held_user: bool, presenting: bool) -> Arc<Self> {
            Arc::new(Self {
                held_user,
                presenting,
                fetches: AtomicUsize::new(0),
            })
        }
    }
    #[async_trait::async_trait]
    impl TokenProvider for RefusingProvider {
        async fn token(&self) -> Result<String, BusError> {
            unreachable!("a request fetches through token_for_request")
        }
        async fn token_for_request(&self) -> Result<String, (BusError, Option<MintRefusal>)> {
            self.fetches.fetch_add(1, Ordering::SeqCst);
            Err((
                BusError::Config(HELD_USER_REFUSAL.into()),
                Some(MintRefusal {
                    message: HELD_USER_REFUSAL.into(),
                    held_user: self.held_user,
                }),
            ))
        }
        fn invalidate(&self) {}
        fn presenting_certificate(&self) -> bool {
            self.presenting
        }
    }

    /// A plain TCP listener on loopback that counts the connections it accepts and
    /// answers none: a request never sent leaves the count at zero.
    async fn counting_listener() -> (String, Arc<AtomicUsize>, tokio::task::JoinHandle<()>) {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}", listener.local_addr().unwrap());
        let accepts = Arc::new(AtomicUsize::new(0));
        let counted = accepts.clone();
        let task = tokio::spawn(async move {
            let mut held = Vec::new();
            while let Ok((sock, _)) = listener.accept().await {
                counted.fetch_add(1, Ordering::SeqCst);
                held.push(sock); // never answered; dropped with the task
            }
        });
        (url, accepts, task)
    }

    fn client_with(url: &str, provider: Option<Arc<dyn TokenProvider>>) -> BusClient {
        BusClient::new(
            url.to_string(),
            String::new(),
            provider,
            None,
            None,
            silent_log(),
        )
        .unwrap()
        .with_test_backoff(Duration::from_millis(5), Duration::from_millis(20))
        .with_test_respond_timing(Duration::from_millis(200), Duration::from_secs(1))
    }

    /// When a request's own mint is refused by the held-user rule and nothing is held to
    /// present, nothing is sent: the subscription retries on its backoff without ever
    /// connecting, its status says the refusal alone, and a response POST fails with the
    /// refusal alone.
    #[tokio::test]
    async fn a_held_user_refusal_with_nothing_held_sends_nothing() {
        let (url, accepts, task) = counting_listener().await;
        let provider = RefusingProvider::new(true, false);
        let client = client_with(&url, Some(provider.clone()));

        let (tx, rx) = shutdown_pair();
        let sub = client.subscribe("ssh-agent", rx);
        let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
        while provider.fetches.load(Ordering::SeqCst) < 3 {
            assert_eq!(
                accepts.load(Ordering::SeqCst),
                0,
                "the subscription made no connection"
            );
            assert!(
                tokio::time::Instant::now() < deadline,
                "the subscription never retried"
            );
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        assert_eq!(
            sub.status().last_error,
            HELD_USER_REFUSAL,
            "the refusal alone"
        );
        let _ = tx.send(true);
        drop(sub);

        let env = Envelope::new_response("r", "ssh-agent", serde_json::Value::Null);
        match client.respond("ssh-agent", &env, &never_shutdown()).await {
            Err(BusError::Config(m)) => assert_eq!(m, HELD_USER_REFUSAL, "the refusal alone"),
            other => panic!("want the refusal, got {other:?}"),
        }

        tokio::time::sleep(Duration::from_millis(50)).await;
        assert_eq!(accepts.load(Ordering::SeqCst), 0, "no connection was made");
        task.abort();
    }

    /// Every other case is sent as before: a server that needs no credential, a refusal
    /// that is not the held-user rule's, and a held-user refusal while a certificate for
    /// the held user is still armed. Both the subscription and a response POST connect.
    #[tokio::test]
    async fn every_other_case_is_still_sent() {
        let cases: Vec<Option<Arc<dyn TokenProvider>>> = vec![
            None,
            Some(RefusingProvider::new(false, false)),
            Some(RefusingProvider::new(true, true)),
        ];
        for (i, provider) in cases.into_iter().enumerate() {
            let (url, accepts, task) = counting_listener().await;
            let client = client_with(&url, provider);
            let (tx, rx) = shutdown_pair();
            let sub = client.subscribe("ssh-agent", rx);
            let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
            while accepts.load(Ordering::SeqCst) == 0 {
                assert!(
                    tokio::time::Instant::now() < deadline,
                    "case {i}: the subscription never connected"
                );
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
            let _ = tx.send(true);
            drop(sub);

            let before = accepts.load(Ordering::SeqCst);
            let env = Envelope::new_response("r", "ssh-agent", serde_json::Value::Null);
            // The listener answers nothing: the POST times out, having connected.
            assert!(client
                .respond("ssh-agent", &env, &never_shutdown())
                .await
                .is_err());
            assert!(
                accepts.load(Ordering::SeqCst) > before,
                "case {i}: the response POST never connected"
            );
            task.abort();
        }
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
        let client = open_client_with(&server.base_url(), log.clone())
            .with_test_backoff(Duration::from_millis(5), Duration::from_millis(10));
        let (tx, rx) = shutdown_pair();
        let sub = client.subscribe("ssh-agent", rx);
        // Poll (bounded) until the quiet DEBUG tier has engaged — ≥1 DEBUG retry
        // implies the single loud WARN already fired and the dedup latched. A
        // fixed sleep here is scheduler-dependent: under CPU oversubscription
        // (e.g. --test-threads=16 on 4 vCPUs) 80 ms of wall clock may not fit
        // even one HTTP round-trip, which failed this test 12/12 in the rehab's
        // stress loop.
        let deadline = tokio::time::Instant::now() + Duration::from_secs(10);
        while log.debugs.load(Ordering::SeqCst) < 1 {
            assert!(
                tokio::time::Instant::now() < deadline,
                "quiet DEBUG tier never engaged (warns={}, debugs={})",
                log.warns.load(Ordering::SeqCst),
                log.debugs.load(Ordering::SeqCst)
            );
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        let _ = tx.send(true);
        drop(sub);
        // Once a DEBUG retry has been observed, the counts are deterministic:
        // the dedup guarantees exactly one WARN ever (first failure), so no
        // post-shutdown drain sleep is needed.
        assert_eq!(
            log.warns.load(Ordering::SeqCst),
            1,
            "WARN loud exactly once"
        );
        assert!(
            log.debugs.load(Ordering::SeqCst) >= 1,
            "subsequent retries drop to the quiet DEBUG tier"
        );
        // Both tiers name the server — here the unnamed client's base URL.
        let server_field = format!(" server={} ", server.base_url());
        let lines = log.lines();
        let warn = lines.iter().find(|l| l.starts_with("WARN ")).unwrap();
        assert!(
            warn.contains("SSE connection lost, reconnecting namespace=ssh-agent")
                && warn.contains(&server_field),
            "{warn}"
        );
        let debug = lines.iter().find(|l| l.starts_with("DEBUG ")).unwrap();
        assert!(debug.contains(&server_field), "{debug}");
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
        assert_eq!(
            parse_operation(&Some(json!({"operation":"list"}))),
            Ok("list".into())
        );
        // Object without operation, or operation null, or a bare null → "" (Go's zero).
        assert_eq!(parse_operation(&Some(json!({}))), Ok(String::new()));
        assert_eq!(
            parse_operation(&Some(json!({"operation":null}))),
            Ok(String::new())
        );
        assert_eq!(
            parse_operation(&Some(serde_json::Value::Null)),
            Ok(String::new())
        );
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

    // ---- the sign flow: the signing check, then the gate, then the backend ----
    //
    // Plan 035 §3.5: the check runs before the gate, so a declined request costs no
    // prompt, touches no approval cache (the cache lives inside the gate, which is never
    // called) and never reaches the backend; it is audited `refused` with its class.

    /// An approval gate that counts its calls and answers a fixed verdict.
    struct CountingGate {
        approve: bool,
        calls: AtomicUsize,
    }
    impl CountingGate {
        fn new(approve: bool) -> Arc<CountingGate> {
            Arc::new(CountingGate {
                approve,
                calls: AtomicUsize::new(0),
            })
        }
        fn calls(&self) -> usize {
            self.calls.load(Ordering::SeqCst)
        }
    }
    #[async_trait::async_trait]
    impl ApprovalGate for CountingGate {
        async fn approve(
            &self,
            _ns: &str,
            _op: &str,
            _server: &str,
            _shed: &str,
            _detail: &str,
        ) -> crate::approval::ApprovalOutcome {
            self.calls.fetch_add(1, Ordering::SeqCst);
            crate::approval::ApprovalOutcome {
                approved: self.approve,
                decided_by: String::new(),
                scope: None,
                ttl: None,
                reason: String::new(),
            }
        }
        fn method(&self) -> &str {
            if self.approve {
                "approve-all"
            } else {
                "deny-all"
            }
        }
    }

    /// One `sign` call: (key, data, flags).
    type SignCall = (Vec<u8>, Vec<u8>, u32);

    /// A backend that records every `sign` call's (key, data, flags) and answers `result`.
    struct RecordingBackend {
        result: Result<SshSignature, String>,
        calls: Mutex<Vec<SignCall>>,
    }
    impl RecordingBackend {
        fn signing() -> Arc<RecordingBackend> {
            Arc::new(RecordingBackend {
                result: Ok(SshSignature {
                    format: "ssh-ed25519".to_string(),
                    blob: vec![0xAB; 64],
                }),
                calls: Mutex::new(Vec::new()),
            })
        }
        fn failing() -> Arc<RecordingBackend> {
            Arc::new(RecordingBackend {
                result: Err("key not found".to_string()),
                calls: Mutex::new(Vec::new()),
            })
        }
        fn calls(&self) -> Vec<SignCall> {
            self.calls.lock().unwrap().clone()
        }
    }
    impl SshBackend for RecordingBackend {
        fn list(&self) -> Result<Vec<SshKeyInfo>, String> {
            Ok(Vec::new())
        }
        fn sign(&self, public_key: &[u8], data: &[u8], flags: u32) -> Result<SshSignature, String> {
            self.calls
                .lock()
                .unwrap()
                .push((public_key.to_vec(), data.to_vec(), flags));
            self.result.clone()
        }
        fn mode(&self) -> &str {
            "local-keys"
        }
    }

    fn put(out: &mut Vec<u8>, bytes: &[u8]) {
        crate::ssh_wire::put_string(out, bytes);
    }

    /// An exact `publickey` login request for `user`, naming `key` with `algorithm`.
    fn login_as(user: &[u8], key: &[u8], algorithm: &[u8]) -> Vec<u8> {
        let mut d = Vec::new();
        put(&mut d, &[7u8; 32]); // session id (SHA-256 sized)
        d.push(50); // SSH_MSG_USERAUTH_REQUEST
        put(&mut d, user);
        put(&mut d, b"ssh-connection");
        put(&mut d, b"publickey");
        d.push(1); // a signature follows
        put(&mut d, algorithm);
        put(&mut d, key);
        d
    }

    /// An exact login as `git` with the fixed ed25519 key.
    fn git_login() -> Vec<u8> {
        login_as(b"git", &fixed_ed25519_pub(), b"ssh-ed25519")
    }

    /// An exact SSHSIG signed-data blob for `namespace` (sha512 of a fixed message).
    fn sshsig(namespace: &[u8]) -> Vec<u8> {
        use sha2::Digest as _;
        let mut d = b"SSHSIG".to_vec();
        put(&mut d, namespace);
        put(&mut d, b"");
        put(&mut d, b"sha512");
        put(&mut d, &sha2::Sha512::digest(b"commit"));
        d
    }

    /// An `ssh-rsa` blob with exponent `e` and a 2048-bit odd modulus.
    fn rsa_pub(e: &[u8]) -> Vec<u8> {
        let mut n = vec![0u8; 1];
        n.extend(std::iter::repeat_n(0xa5u8, 256));
        n[1] |= 0x80;
        *n.last_mut().unwrap() |= 1;
        let mut b = Vec::new();
        put(&mut b, b"ssh-rsa");
        put(&mut b, e);
        put(&mut b, &n);
        b
    }

    /// Run `handle_sign` with counting doubles.
    async fn run_sign(
        env: &Envelope,
        policy: &SignPolicy,
        gate: &Arc<CountingGate>,
        backend: &Arc<RecordingBackend>,
    ) -> (serde_json::Value, AuditEntry) {
        let gate_dyn: Arc<dyn ApprovalGate> = gate.clone();
        let backend_dyn: Arc<dyn SshBackend> = backend.clone();
        handle_sign(env, "", "web", policy, &gate_dyn, &backend_dyn).await
    }

    #[tokio::test]
    async fn an_accepted_login_signs_once_with_the_checked_buffers() {
        let key = fixed_ed25519_pub();
        let data = git_login();
        let env = sign_env(&b64(&key), &b64(&data), 0);
        let (gate, backend) = (CountingGate::new(true), RecordingBackend::signing());
        let (payload, entry) = run_sign(&env, &SignPolicy::default(), &gate, &backend).await;

        // SSHSignResponse{format, blob(b64 sig), rest:""}.
        assert_eq!(payload["format"], "ssh-ed25519");
        assert_eq!(payload["blob"], b64(&[0xAB; 64]));
        assert_eq!(payload["rest"], "");
        // Exactly one backend call, with exactly the decoded buffers and flags.
        assert_eq!(backend.calls(), vec![(key, data, 0)]);
        assert_eq!(gate.calls(), 1);
        // Today's ok audit: detail=key type, approval=approve-all, nothing else.
        assert_eq!(
            entry,
            AuditEntry {
                shed: "web".into(),
                ns: "ssh-agent".into(),
                op: "sign".into(),
                result: "ok".into(),
                detail: "ssh-ed25519".into(),
                approval: "approve-all".into(),
                ..Default::default()
            }
        );
    }

    #[tokio::test]
    async fn an_accepted_git_sshsig_signs_once_with_the_checked_buffers() {
        let key = fixed_ed25519_pub();
        let data = sshsig(b"git");
        let env = sign_env(&b64(&key), &b64(&data), 0);
        let (gate, backend) = (CountingGate::new(true), RecordingBackend::signing());
        let (payload, entry) = run_sign(&env, &SignPolicy::default(), &gate, &backend).await;
        assert_eq!(payload["format"], "ssh-ed25519");
        assert_eq!(backend.calls(), vec![(key, data, 0)]);
        assert_eq!(gate.calls(), 1);
        assert_eq!(entry.result, "ok");
        assert_eq!(entry.detail, "ssh-ed25519");
        assert_eq!(entry.code, "");
        assert_eq!(entry.reason, "");
    }

    /// An RSA login passes its flags through the check to the backend unchanged.
    #[tokio::test]
    async fn an_accepted_rsa_login_signs_with_the_checked_flags() {
        let key = rsa_pub(&[1, 0, 1]);
        let data = login_as(b"git", &key, b"rsa-sha2-512");
        let env = sign_env(&b64(&key), &b64(&data), 4);
        let (gate, backend) = (CountingGate::new(true), RecordingBackend::signing());
        let (_, entry) = run_sign(&env, &SignPolicy::default(), &gate, &backend).await;
        assert_eq!(backend.calls(), vec![(key, data, 4)]);
        assert_eq!(entry.result, "ok");
        assert_eq!(entry.detail, "ssh-rsa");
    }

    #[tokio::test]
    async fn sign_deny_returns_error_and_denied_audit() {
        let env = sign_env(&b64(&fixed_ed25519_pub()), &b64(&git_login()), 0);
        let (gate, backend) = (CountingGate::new(false), RecordingBackend::signing());
        let (payload, entry) = run_sign(&env, &SignPolicy::default(), &gate, &backend).await;
        // {approval denied, SIGN_FAILED}.
        assert_eq!(payload["error"], "approval denied");
        assert_eq!(payload["code"], "SIGN_FAILED");
        assert_eq!(gate.calls(), 1);
        assert!(backend.calls().is_empty(), "a denial signs nothing");
        // denied audit: result=denied, approval=deny-all; NO detail, NO code, NO reason.
        assert_eq!(
            entry,
            AuditEntry {
                shed: "web".into(),
                ns: "ssh-agent".into(),
                op: "sign".into(),
                result: "denied".into(),
                approval: "deny-all".into(),
                ..Default::default()
            }
        );
    }

    #[tokio::test]
    async fn sign_backend_error_sign_failed_and_error_audit() {
        // A valid login with a key the backend does not hold -> key not found ->
        // {sign operation failed, SIGN_FAILED} + error audit (detail=key type).
        let env = sign_env(&b64(&fixed_ed25519_pub()), &b64(&git_login()), 0);
        let (gate, backend) = (CountingGate::new(true), RecordingBackend::failing());
        let (payload, entry) = run_sign(&env, &SignPolicy::default(), &gate, &backend).await;
        assert_eq!(payload["error"], "sign operation failed");
        assert_eq!(payload["code"], "SIGN_FAILED");
        assert_eq!(backend.calls().len(), 1);
        assert_eq!(entry.result, "error");
        assert_eq!(entry.detail, "ssh-ed25519");
        assert_eq!(entry.approval, "approve-all");
        assert_eq!(entry.code, ""); // ssh_handler.go's error audit sets no code
    }

    /// One declined request per refusal class (and per envelope decode step): no gate
    /// call, no backend call, the reply the shed could always get, and one `refused`
    /// audit entry naming the class with a bounded detail that is not the request.
    #[tokio::test]
    async fn every_refusal_class_skips_the_gate_and_the_backend_and_is_audited() {
        let key = fixed_ed25519_pub();
        let other = rsa_pub(&[1, 0, 1]);
        let mut only_other = crate::sign_check::KeyFilter::new();
        only_other.insert_key_blob(&other);
        let filtered = SignPolicy::new(["git"], Some(only_other));
        // `_bootstrap` listed on purpose: a reserved name is refused even when listed.
        let listing_reserved = SignPolicy::new(["git", "_bootstrap"], None);
        let default = SignPolicy::default();
        let mut trailing = git_login();
        trailing.push(0);
        let rsa_512 = login_as(b"git", &other, b"rsa-sha2-512");
        let even_e = rsa_pub(&[1, 0, 0]);
        let bad_flags = Envelope {
            payload: Some(serde_json::json!({"operation":"sign","flags":"not-a-number"})),
            ..sign_env("", "", 0)
        };

        let invalid_request = ("invalid sign request", "INTERNAL_ERROR");
        let sign_failed = ("sign operation failed", "SIGN_FAILED");
        #[allow(clippy::type_complexity)]
        let cases: Vec<(
            &str,
            Envelope,
            &SignPolicy,
            RefusalClass,
            (&str, &str),
            String,
        )> = vec![
            (
                "envelope does not decode",
                bad_flags,
                &default,
                RefusalClass::MalformedRequest,
                invalid_request,
                "invalid\\x20sign\\x20request".into(),
            ),
            (
                "key is not base64",
                sign_env("!!!not-base64!!!", &b64(&git_login()), 0),
                &default,
                RefusalClass::MalformedRequest,
                ("invalid public key encoding", "INTERNAL_ERROR"),
                "invalid\\x20public\\x20key\\x20encoding".into(),
            ),
            (
                "data is not base64",
                sign_env(&b64(&key), "!!!bad!!!", 0),
                &default,
                RefusalClass::MalformedRequest,
                ("invalid challenge data encoding", "INTERNAL_ERROR"),
                "invalid\\x20challenge\\x20data\\x20encoding".into(),
            ),
            (
                "key does not parse",
                sign_env(&b64(b"definitely not an ssh key"), &b64(&git_login()), 0),
                &default,
                RefusalClass::MalformedKey,
                ("invalid public key", "KEY_NOT_FOUND"),
                "not an SSH public key".into(),
            ),
            (
                "key parses but is not a valid key (even RSA exponent)",
                sign_env(
                    &b64(&even_e),
                    &b64(&login_as(b"git", &even_e, b"ssh-rsa")),
                    0,
                ),
                &default,
                RefusalClass::MalformedKey,
                sign_failed,
                "invalid key: RSA exponent is even".into(),
            ),
            (
                "key not offered",
                sign_env(&b64(&key), &b64(&git_login()), 0),
                &filtered,
                RefusalClass::KeyNotOffered,
                sign_failed,
                format!(
                    "key {}",
                    crate::sign_check::digest_fingerprint(&crate::sign_check::key_blob_digest(
                        &key
                    ))
                ),
            ),
            (
                "username not allowed",
                sign_env(
                    &b64(&key),
                    &b64(&login_as(b"deploy", &key, b"ssh-ed25519")),
                    0,
                ),
                &default,
                RefusalClass::UsernameNotAllowed,
                sign_failed,
                "login user=deploy".into(),
            ),
            (
                "reserved username, even when listed",
                sign_env(
                    &b64(&key),
                    &b64(&login_as(b"_bootstrap", &key, b"ssh-ed25519")),
                    0,
                ),
                &listing_reserved,
                RefusalClass::ReservedUsername,
                sign_failed,
                "login user=_bootstrap".into(),
            ),
            (
                "wrong SSHSIG namespace",
                sign_env(&b64(&key), &b64(&sshsig(b"file")), 0),
                &default,
                RefusalClass::WrongNamespace,
                sign_failed,
                "sshsig namespace=file".into(),
            ),
            (
                "arbitrary challenge bytes",
                sign_env(&b64(&key), &b64(b"challenge-bytes"), 0),
                &default,
                RefusalClass::UnparseableData,
                sign_failed,
                String::new(),
            ),
            (
                "a trailing byte after a valid login",
                sign_env(&b64(&key), &b64(&trailing), 0),
                &default,
                RefusalClass::UnparseableData,
                sign_failed,
                String::new(),
            ),
            (
                "RSA flags disagree with the payload",
                sign_env(&b64(&other), &b64(&rsa_512), 2),
                &default,
                RefusalClass::UnparseableData,
                sign_failed,
                "login: RSA flags do not name the algorithm's hash".into(),
            ),
        ];
        let mut classes = std::collections::HashSet::new();
        for (name, env, policy, class, (error, code), detail) in cases {
            for approve in [true, false] {
                let (gate, backend) = (CountingGate::new(approve), RecordingBackend::signing());
                let (payload, entry) = run_sign(&env, policy, &gate, &backend).await;
                assert_eq!(gate.calls(), 0, "{name}: the gate was asked");
                assert!(backend.calls().is_empty(), "{name}: the backend was asked");
                assert_eq!(
                    payload,
                    serde_json::json!({"error": error, "code": code}),
                    "{name}"
                );
                assert_eq!(entry.result, "refused", "{name}");
                assert_eq!(entry.code, "SIGN_REFUSED", "{name}");
                assert_eq!(entry.reason, class.as_str(), "{name}");
                assert_eq!(entry.approval, "none", "{name}");
                assert_eq!(
                    (entry.ns.as_str(), entry.op.as_str()),
                    ("ssh-agent", "sign")
                );
                assert_eq!(entry.shed, "web", "{name}");
                assert!(
                    entry.decided_by.is_empty() && entry.scope.is_empty() && entry.ttl.is_empty(),
                    "{name}: {entry:?}"
                );
                if detail.is_empty() {
                    assert!(!entry.detail.is_empty(), "{name}: no detail");
                } else {
                    assert_eq!(entry.detail, detail, "{name}");
                }
                assert!(
                    entry.detail.len() <= crate::sign_check::MAX_DETAIL_BYTES,
                    "{name}"
                );
                // Never the raw request: neither encoded field appears in the line.
                let raw = env.payload.as_ref().unwrap();
                for field in ["public_key", "data"] {
                    if let Some(v) = raw.get(field).and_then(|v| v.as_str()) {
                        if v.len() > 8 {
                            assert!(!entry.detail.contains(v), "{name}: {field} in detail");
                        }
                    }
                }
            }
            classes.insert(class);
        }
        // Every class the check (or the decode before it) can produce is covered.
        assert_eq!(classes.len(), RefusalClass::ALL.len(), "{classes:?}");
    }

    /// The old order ran the gate first, so a malformed key under a deny-all gate answered
    /// `approval denied`. Now the key is checked first: the key error, and no prompt.
    #[tokio::test]
    async fn a_malformed_key_under_a_deny_gate_answers_the_key_error() {
        let gate = CountingGate::new(false);
        let backend = RecordingBackend::signing();
        let env = sign_env(&b64(b"definitely not an ssh key"), &b64(&git_login()), 0);
        let (payload, entry) = run_sign(&env, &SignPolicy::default(), &gate, &backend).await;
        assert_eq!(payload["error"], "invalid public key");
        assert_eq!(payload["code"], "KEY_NOT_FOUND");
        assert_eq!(entry.result, "refused");
        assert_eq!(entry.reason, "malformed_key");
        let env = sign_env("!!!not-base64!!!", &b64(&git_login()), 0);
        let (payload, _) = run_sign(&env, &SignPolicy::default(), &gate, &backend).await;
        assert_eq!(payload["error"], "invalid public key encoding");
        assert_eq!(payload["code"], "INTERNAL_ERROR");
        assert_eq!(gate.calls(), 0, "no prompt for a malformed key");
    }

    /// The check runs before the gate: a declined request is never put to the gate (so no
    /// prompt and no approval-cache entry read or written), an accepted one is, once.
    #[tokio::test]
    async fn sign_check_runs_before_gate() {
        let gate = CountingGate::new(true);
        let backend = RecordingBackend::signing();
        let key = b64(&fixed_ed25519_pub());
        let declined = sign_env(&key, &b64(b"shed-host-agent challenge"), 0);
        let (payload, _) = run_sign(&declined, &SignPolicy::default(), &gate, &backend).await;
        assert_eq!(payload["code"], "SIGN_FAILED");
        assert_eq!(gate.calls(), 0);
        let accepted = sign_env(&key, &b64(&git_login()), 0);
        let (payload, _) = run_sign(&accepted, &SignPolicy::default(), &gate, &backend).await;
        assert_eq!(payload["format"], "ssh-ed25519");
        assert_eq!(gate.calls(), 1);
        assert_eq!(backend.calls().len(), 1);
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
        let env = sign_env(&b64(&pub_bytes), &b64(&git_login()), 0);
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

    /// A refusal through the shared dispatch: the shed gets the ordinary failure shape
    /// and the sink gets the `refused` line.
    #[tokio::test]
    async fn a_refusal_via_handle_bus_message_responds_sign_failed_and_audits() {
        let server = MockServer::start_async().await;
        let respond = server
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/plugins/listeners/ssh-agent/respond")
                    .body_contains("\"in_reply_to\":\"sign-1\"")
                    .body_contains("\"code\":\"SIGN_FAILED\"")
                    .body_contains("\"error\":\"sign operation failed\"");
                t.status(204);
            })
            .await;
        let client = open_client(&server.base_url());
        let audit_collector = Arc::new(CollectingAudit::default());
        let gate = CountingGate::new(true);
        let env = sign_env(
            &b64(&fixed_ed25519_pub()),
            &b64(&login_as(b"root", &fixed_ed25519_pub(), b"ssh-ed25519")),
            0,
        );
        handle_bus_message(
            &client,
            "ssh-agent",
            &env,
            &never_shutdown(),
            &test_handlers(gate.clone(), audit_collector.clone(), empty_backend()),
        )
        .await;
        respond.assert_async().await;
        assert_eq!(gate.calls(), 0);
        let entries = audit_collector.entries.lock().unwrap();
        assert_eq!(
            *entries,
            vec![AuditEntry {
                shed: "web".into(),
                ns: "ssh-agent".into(),
                op: "sign".into(),
                result: "refused".into(),
                detail: "login user=root".into(),
                code: "SIGN_REFUSED".into(),
                reason: "username_not_allowed".into(),
                approval: "none".into(),
                ..Default::default()
            }]
        );
    }

    // ---- the refusal audit limit ----

    /// A hand-driven clock and timer for the limiter: time moves only on `advance`.
    struct ManualClock {
        now: watch::Sender<Instant>,
    }
    impl ManualClock {
        fn new() -> Arc<ManualClock> {
            Arc::new(ManualClock {
                now: watch::channel(Instant::now()).0,
            })
        }
        fn advance(&self, by: Duration) {
            self.now.send_modify(|t| *t += by);
        }
    }
    impl RefusalClock for ManualClock {
        fn now(&self) -> Instant {
            *self.now.borrow()
        }
        fn sleep_until(
            &self,
            at: Instant,
        ) -> std::pin::Pin<Box<dyn std::future::Future<Output = ()> + Send + 'static>> {
            let mut rx = self.now.subscribe();
            Box::pin(async move {
                let _ = rx.wait_for(|t| *t >= at).await;
            })
        }
    }

    const MINUTE: Duration = Duration::from_secs(60);

    fn class_counts(pairs: &[(RefusalClass, u64)]) -> [u64; REFUSAL_CLASSES] {
        let mut counts = [0; REFUSAL_CLASSES];
        for (class, n) in pairs {
            counts[class_index(*class)] += n;
        }
        counts
    }

    fn add_counts(into: &mut [u64; REFUSAL_CLASSES], from: &[u64; REFUSAL_CLASSES]) {
        for (n, m) in into.iter_mut().zip(from) {
            *n += m;
        }
    }

    fn summary_of(shed: &str, pairs: &[(RefusalClass, u64)]) -> RefusalSummary {
        RefusalSummary {
            shed: shed.into(),
            counts: class_counts(pairs),
        }
    }

    fn logs(log: bool) -> RefusalAdmission {
        RefusalAdmission {
            log,
            summaries: vec![],
        }
    }

    /// Wait (on the scheduler, not on a deadline that matters) until `pred` holds for
    /// the collected entries.
    async fn wait_for_audit(audit: &CollectingAudit, pred: impl Fn(&[AuditEntry]) -> bool) {
        for _ in 0..2000 {
            if pred(&audit.entries.lock().unwrap()) {
                return;
            }
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        panic!("never reached: {:?}", audit.entries.lock().unwrap());
    }

    fn summaries_in(entries: &[AuditEntry]) -> Vec<AuditEntry> {
        entries
            .iter()
            .filter(|e| e.reason == AUDIT_REASON_RATE_LIMITED)
            .cloned()
            .collect()
    }

    #[test]
    fn the_refusal_limiter_logs_a_burst_then_counts_per_class_and_reports_once() {
        use RefusalClass::*;
        let limiter = RefusalAuditLimiter::new(3, MINUTE);
        let t0 = Instant::now();
        for _ in 0..3 {
            assert_eq!(limiter.admit("web", UnparseableData, t0), logs(true));
        }
        // Past the burst: the same class is counted ...
        assert_eq!(limiter.admit("web", UnparseableData, t0), logs(false));
        // ... but the first of a class not yet logged in this window gets its line.
        assert_eq!(limiter.admit("web", ReservedUsername, t0), logs(true));
        assert_eq!(limiter.admit("web", ReservedUsername, t0), logs(false));
        for _ in 0..3 {
            assert_eq!(
                limiter.admit("web", UnparseableData, t0 + Duration::from_secs(1)),
                logs(false)
            );
        }
        // Another shed has its own window.
        assert_eq!(limiter.admit("db", ReservedUsername, t0), logs(true));
        // Not before the window ends.
        assert_eq!(limiter.flush_due(t0 + Duration::from_secs(59)), vec![]);
        // At its end: the counts, per class, once; the windows are dropped.
        assert_eq!(
            limiter.flush_due(t0 + MINUTE),
            vec![summary_of(
                "web",
                &[(ReservedUsername, 1), (UnparseableData, 4)]
            )]
        );
        assert_eq!(limiter.flush_due(t0 + MINUTE * 2), vec![]);
        assert_eq!(limiter.windows(), 0);
        // A new window: logged again.
        assert_eq!(
            limiter.admit("web", UnparseableData, t0 + MINUTE),
            logs(true)
        );
    }

    /// The reviewer's case: a flood of one class spends the burst, then a well-formed
    /// login as a reserved username still leaves a line of its own, and every request is
    /// either logged or counted under its own class. Per window, at most burst + one line
    /// per class + one summary.
    #[test]
    fn mixed_classes_under_suppression_keep_their_class_within_the_bound() {
        let burst = 2;
        let limiter = RefusalAuditLimiter::new(burst, MINUTE);
        let t0 = Instant::now();
        let mut logged = [0u64; REFUSAL_CLASSES];
        let mut sent = [0u64; REFUSAL_CLASSES];
        let mut summaries = Vec::new();
        for window in 0..3u32 {
            let at = t0 + MINUTE * window;
            let mut lines = 0;
            for _ in 0..100 {
                for i in 0..REFUSAL_CLASSES {
                    // Rotate which classes come first, so each window's burst differs.
                    let class = RefusalClass::ALL[(i + window as usize) % REFUSAL_CLASSES];
                    let a = limiter.admit("web", class, at);
                    summaries.extend(a.summaries);
                    sent[class_index(class)] += 1;
                    if a.log {
                        logged[class_index(class)] += 1;
                        lines += 1;
                    }
                }
            }
            // The burst, then the first of each class not in the burst: 2 + 5.
            assert_eq!(lines, burst as usize + REFUSAL_CLASSES - burst as usize);
            assert!(lines <= burst as usize + REFUSAL_CLASSES);
        }
        summaries.extend(limiter.flush_due(t0 + MINUTE * 3));
        // One summary per window, each naming every class.
        assert_eq!(summaries.len(), 3);
        for s in &summaries {
            assert!(s.counts.iter().all(|n| *n > 0), "{s:?}");
            assert_eq!(s.total(), 700 - 7);
        }
        for i in 0..REFUSAL_CLASSES {
            let counted: u64 = summaries.iter().map(|s| s.counts[i]).sum();
            assert_eq!(logged[i] + counted, sent[i], "class {i}");
        }
        // The summary line names each class with its count, in a stable order.
        let line = limiter.summary_audit("srv", &summaries[0]);
        assert_eq!(
            line.detail,
            "693 more refused sign requests were not logged one by one (at most 2 per 60s \
             per shed, plus the first of each class): malformed_request=99 malformed_key=99 \
             key_not_offered=99 username_not_allowed=99 reserved_username=99 \
             wrong_namespace=99 unparseable_data=99"
        );
        assert_eq!(
            (
                line.server.as_str(),
                line.shed.as_str(),
                line.result.as_str(),
                line.code.as_str(),
                line.reason.as_str(),
                line.approval.as_str()
            ),
            (
                "srv",
                "web",
                "refused",
                "SIGN_REFUSED",
                AUDIT_REASON_RATE_LIMITED,
                "none"
            )
        );
    }

    #[test]
    fn a_refusal_after_the_window_reports_the_ended_windows_counts() {
        use RefusalClass::*;
        let limiter = RefusalAuditLimiter::new(1, MINUTE);
        let t0 = Instant::now();
        assert_eq!(limiter.admit("web", WrongNamespace, t0), logs(true));
        assert_eq!(limiter.admit("web", WrongNamespace, t0), logs(false));
        assert_eq!(limiter.admit("web", WrongNamespace, t0), logs(false));
        assert_eq!(
            limiter.admit("web", KeyNotOffered, t0 + MINUTE),
            RefusalAdmission {
                log: true,
                summaries: vec![summary_of("web", &[(WrongNamespace, 2)])],
            }
        );
        // The counts were taken: a late flush finds nothing.
        assert_eq!(limiter.flush_due(t0 + MINUTE * 3), vec![]);
    }

    /// The window map is capped: a new shed past the cap drops ended windows, then the
    /// oldest open one, and the counts of each come back as its summary.
    #[test]
    fn the_window_cap_evicts_the_oldest_and_keeps_its_counts() {
        use RefusalClass::*;
        let clock = ManualClock::new();
        let limiter = RefusalAuditLimiter::with_clock(0, MINUTE, 3, clock.clone());
        let t0 = clock.now();
        for (i, shed) in ["a", "b", "c"].iter().enumerate() {
            let at = t0 + Duration::from_secs(i as u64);
            assert_eq!(limiter.admit(shed, UnparseableData, at), logs(true));
            assert_eq!(limiter.admit(shed, UnparseableData, at), logs(false));
            assert_eq!(limiter.admit(shed, UnparseableData, at), logs(false));
        }
        assert_eq!(limiter.windows(), 3);
        // "d" evicts the oldest ("a"), whose two counted refusals are reported.
        assert_eq!(
            limiter.admit("d", ReservedUsername, t0 + Duration::from_secs(3)),
            RefusalAdmission {
                log: true,
                summaries: vec![summary_of("a", &[(UnparseableData, 2)])],
            }
        );
        assert_eq!(limiter.windows(), 3);
        // Past every window's end, a new shed drops all the ended windows at once.
        let mut got = limiter
            .admit("e", ReservedUsername, t0 + MINUTE * 2)
            .summaries;
        got.sort_by(|x, y| x.shed.cmp(&y.shed));
        assert_eq!(
            got,
            vec![
                summary_of("b", &[(UnparseableData, 2)]),
                summary_of("c", &[(UnparseableData, 2)]),
            ]
        );
        assert_eq!(limiter.windows(), 1);
    }

    /// Many shed names against a small cap: the map never grows past it, and every
    /// refusal is logged or counted under its class exactly once.
    #[test]
    fn many_shed_names_stay_under_the_cap_and_lose_no_count() {
        let cap = 8;
        let limiter = RefusalAuditLimiter::with_clock(1, MINUTE, cap, ManualClock::new());
        let t0 = Instant::now();
        let mut sent: HashMap<(String, usize), u64> = HashMap::new();
        let mut seen: HashMap<(String, usize), u64> = HashMap::new();
        fn account(summaries: Vec<RefusalSummary>, seen: &mut HashMap<(String, usize), u64>) {
            for s in summaries {
                for (i, n) in s.counts.iter().enumerate() {
                    if *n > 0 {
                        *seen.entry((s.shed.clone(), i)).or_default() += n;
                    }
                }
            }
        }
        for round in 0..3u32 {
            for n in 0..200u32 {
                let shed = format!("shed-{n}");
                let at = t0 + Duration::from_millis(u64::from(round * 200 + n));
                for k in 0..5usize {
                    let class = RefusalClass::ALL[k % 2];
                    let i = class_index(class);
                    *sent.entry((shed.clone(), i)).or_default() += 1;
                    let a = limiter.admit(&shed, class, at);
                    if a.log {
                        *seen.entry((shed.clone(), i)).or_default() += 1;
                    }
                    account(a.summaries, &mut seen);
                    assert!(limiter.windows() <= cap);
                }
            }
        }
        account(limiter.close(), &mut seen);
        assert_eq!(limiter.windows(), 0);
        assert_eq!(seen, sent);
    }

    /// Admissions and flushes racing from many threads (released together by a
    /// barrier, with windows ending part-way through): no refusal is lost or counted
    /// twice. Every decision is taken under the one lock, so whatever the interleaving,
    /// logged + summarized = sent, per class.
    #[test]
    fn concurrent_admissions_and_flushes_lose_and_double_nothing() {
        const THREADS: usize = 8;
        const EACH: u64 = 3000;
        let limiter = Arc::new(RefusalAuditLimiter::new(5, Duration::from_millis(50)));
        let t0 = Instant::now();
        let barrier = Arc::new(std::sync::Barrier::new(THREADS + 1));
        let stop = Arc::new(AtomicBool::new(false));
        let mut workers = Vec::new();
        for t in 0..THREADS {
            let (limiter, barrier) = (limiter.clone(), barrier.clone());
            workers.push(std::thread::spawn(move || {
                let mut logged = [0u64; REFUSAL_CLASSES];
                let mut sent = [0u64; REFUSAL_CLASSES];
                let mut summarized = [0u64; REFUSAL_CLASSES];
                barrier.wait();
                for k in 0..EACH {
                    let class = RefusalClass::ALL[(t + k as usize) % REFUSAL_CLASSES];
                    let shed = ["web", "db", "ci"][k as usize % 3];
                    // Time moves on, so windows end while others are still admitting.
                    let at = t0 + Duration::from_millis(k / 10);
                    let a = limiter.admit(shed, class, at);
                    sent[class_index(class)] += 1;
                    if a.log {
                        logged[class_index(class)] += 1;
                    }
                    for s in a.summaries {
                        add_counts(&mut summarized, &s.counts);
                    }
                }
                (logged, sent, summarized)
            }));
        }
        let flusher = {
            let (limiter, barrier, stop) = (limiter.clone(), barrier.clone(), stop.clone());
            std::thread::spawn(move || {
                let mut summarized = [0u64; REFUSAL_CLASSES];
                barrier.wait();
                let mut k = 0u64;
                while !stop.load(Ordering::SeqCst) {
                    for s in limiter.flush_due(t0 + Duration::from_millis(k / 10)) {
                        add_counts(&mut summarized, &s.counts);
                    }
                    k = (k + 1) % (EACH + 1000);
                }
                summarized
            })
        };
        let mut total = [[0u64; REFUSAL_CLASSES]; 3];
        for w in workers {
            let (logged, sent, summarized) = w.join().unwrap();
            for i in 0..REFUSAL_CLASSES {
                total[0][i] += logged[i];
                total[1][i] += sent[i];
                total[2][i] += summarized[i];
            }
        }
        stop.store(true, Ordering::SeqCst);
        let flushed = flusher.join().unwrap();
        let closed = limiter.close();
        for i in 0..REFUSAL_CLASSES {
            let at_close: u64 = closed.iter().map(|s| s.counts[i]).sum();
            assert_eq!(
                total[0][i] + total[2][i] + flushed[i] + at_close,
                total[1][i],
                "class {i}"
            );
        }
        assert_eq!(total[1].iter().sum::<u64>(), THREADS as u64 * EACH);
    }

    /// The flusher writes a window's summary when the (injected) clock reaches the
    /// window's end — no wall-clock wait — and then drops the window.
    #[tokio::test]
    async fn the_flusher_writes_the_summary_when_the_window_ends() {
        use RefusalClass::*;
        let clock = ManualClock::new();
        let limiter = Arc::new(RefusalAuditLimiter::with_clock(
            1,
            MINUTE,
            16,
            clock.clone(),
        ));
        let audit = Arc::new(CollectingAudit::default());
        let (stop_tx, stop_rx) = shutdown_pair();
        let flusher = tokio::spawn(run_refusal_flusher(
            limiter.clone(),
            audit.clone(),
            "srv".into(),
            stop_rx,
        ));
        // Let the flusher run and park on its timer at each step (a current-thread
        // runtime: yielding runs every task that is ready), so the summary below can
        // only come from the timer firing at the window's end.
        async fn settle() {
            for _ in 0..20 {
                tokio::task::yield_now().await;
            }
        }
        settle().await;
        let t0 = clock.now();
        assert!(limiter.admit("web", UsernameNotAllowed, t0).log);
        for _ in 0..4 {
            assert!(!limiter.admit("web", UsernameNotAllowed, t0).log);
        }
        assert_eq!(limiter.next_deadline(), Some(t0 + MINUTE));
        settle().await;
        clock.advance(Duration::from_secs(59));
        settle().await;
        assert!(summaries_in(&audit.entries.lock().unwrap()).is_empty());
        clock.advance(Duration::from_secs(1));
        wait_for_audit(&audit, |e| !summaries_in(e).is_empty()).await;
        let lines = summaries_in(&audit.entries.lock().unwrap());
        assert_eq!(lines.len(), 1);
        assert!(
            lines[0].detail.starts_with("4 more refused sign requests")
                && lines[0].detail.ends_with(": username_not_allowed=4"),
            "{lines:?}"
        );
        assert_eq!(
            (lines[0].server.as_str(), lines[0].shed.as_str()),
            ("srv", "web")
        );
        wait_for_audit(&audit, |_| limiter.windows() == 0).await;
        // Shutdown stops the flusher; nothing was pending, so nothing more is written.
        stop_tx.send(true).unwrap();
        flusher.await.unwrap();
        assert_eq!(audit.entries.lock().unwrap().len(), 1);
    }

    /// On shutdown the flusher writes every pending summary before it returns (the
    /// group's `done` awaits it), and later refusals are logged, not counted.
    #[tokio::test]
    async fn shutdown_writes_every_pending_summary_then_logs_each_refusal() {
        use RefusalClass::*;
        let clock = ManualClock::new();
        let limiter = Arc::new(RefusalAuditLimiter::with_clock(
            0,
            MINUTE,
            16,
            clock.clone(),
        ));
        let audit = Arc::new(CollectingAudit::default());
        let (stop_tx, stop_rx) = shutdown_pair();
        let flusher = tokio::spawn(run_refusal_flusher(
            limiter.clone(),
            audit.clone(),
            String::new(),
            stop_rx,
        ));
        let t0 = clock.now();
        for shed in ["web", "db"] {
            assert!(limiter.admit(shed, MalformedKey, t0).log);
            assert!(!limiter.admit(shed, MalformedKey, t0).log);
            assert!(!limiter.admit(shed, MalformedKey, t0).log);
        }
        stop_tx.send(true).unwrap();
        flusher.await.unwrap();
        let mut lines = summaries_in(&audit.entries.lock().unwrap());
        lines.sort_by(|a, b| a.shed.cmp(&b.shed));
        assert_eq!(lines.len(), 2, "{lines:?}");
        for (line, shed) in lines.iter().zip(["db", "web"]) {
            assert_eq!(line.shed, shed);
            assert!(line.detail.ends_with(": malformed_key=2"), "{line:?}");
        }
        assert_eq!(limiter.windows(), 0);
        // After the last flush nothing is counted any more.
        assert_eq!(limiter.admit("web", MalformedKey, t0), logs(true));
        assert_eq!(limiter.admit("web", MalformedKey, t0), logs(true));
        assert_eq!(limiter.windows(), 0);
    }

    /// End to end through the bus: a flood of refusals of two classes from one shed
    /// writes the burst, the first line of the second class, and — at shutdown — one
    /// summary with each class's count. Nothing here depends on how long it takes: the
    /// clock never moves.
    #[tokio::test]
    async fn a_flood_of_refusals_writes_the_burst_then_one_summary() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/plugins/listeners/ssh-agent/respond");
                t.status(204);
            })
            .await;
        let client = open_client(&server.base_url());
        let audit_collector = Arc::new(CollectingAudit::default());
        let mut handlers = test_handlers(
            CountingGate::new(true),
            audit_collector.clone(),
            empty_backend(),
        );
        handlers.refusal_audits = Arc::new(RefusalAuditLimiter::with_clock(
            2,
            MINUTE,
            16,
            ManualClock::new(),
        ));
        let (stop_tx, stop_rx) = shutdown_pair();
        let flusher = tokio::spawn(run_refusal_flusher(
            handlers.refusal_audits.clone(),
            handlers.audit.clone(),
            String::new(),
            stop_rx,
        ));
        let key = fixed_ed25519_pub();
        let junk = sign_env(&b64(&key), &b64(b"not a login"), 0);
        let reserved = sign_env(
            &b64(&key),
            &b64(&login_as(b"_bootstrap", &key, b"ssh-ed25519")),
            0,
        );
        for env in std::iter::repeat_n(&junk, 20).chain(std::iter::repeat_n(&reserved, 3)) {
            handle_bus_message(&client, "ssh-agent", env, &never_shutdown(), &handlers).await;
        }
        {
            let entries = audit_collector.entries.lock().unwrap();
            let count = |reason: &str| entries.iter().filter(|e| e.reason == reason).count();
            assert_eq!(count("unparseable_data"), 2);
            assert_eq!(count("reserved_username"), 1, "{entries:?}");
            let reserved_line = entries
                .iter()
                .find(|e| e.reason == "reserved_username")
                .unwrap();
            assert_eq!(reserved_line.detail, "login user=_bootstrap");
            assert!(summaries_in(&entries).is_empty());
        }
        stop_tx.send(true).unwrap();
        flusher.await.unwrap();
        let entries = audit_collector.entries.lock().unwrap();
        let summaries = summaries_in(&entries);
        assert_eq!(summaries.len(), 1, "{entries:?}");
        assert_eq!(entries.len(), 4);
        let summary = &summaries[0];
        assert!(
            summary.detail.starts_with("20 more refused sign requests")
                && summary
                    .detail
                    .ends_with(": reserved_username=2 unparseable_data=18"),
            "{summary:?}"
        );
        assert_eq!(
            (
                summary.result.as_str(),
                summary.code.as_str(),
                summary.approval.as_str()
            ),
            ("refused", "SIGN_REFUSED", "none")
        );
        assert_eq!(summary.shed, "web");
    }

    // ---- the sign request's fields at their edges ----

    /// Each field of a sign request at its edges: the numeric flags (0, the hash bits,
    /// u32 max, one past it, negative, a string, a fraction), a missing or null key, data
    /// or flags, and base64 with more after the padding. Each gets its class (or is
    /// signed), the reply the shed always got, and its audit line; a refused one never
    /// reaches the gate or the backend.
    #[tokio::test]
    async fn sign_request_field_edges_are_classified_and_answered() {
        let ed = fixed_ed25519_pub();
        let rsa = rsa_pub(&[1, 0, 1]);
        let ed_login = git_login();
        let rsa_login = login_as(b"git", &rsa, b"rsa-sha2-512");
        let env = |fields: serde_json::Value| {
            let mut payload = serde_json::json!({"operation": "sign"});
            for (k, v) in fields.as_object().unwrap() {
                payload[k] = v.clone();
            }
            Envelope {
                payload: Some(payload),
                ..sign_env("", "", 0)
            }
        };
        let with_flags = |key: &[u8], data: &[u8], flags: serde_json::Value| {
            env(serde_json::json!({"public_key": b64(key), "data": b64(data), "flags": flags}))
        };
        // The login's base64 ends in padding; more after it does not decode.
        assert!(b64(&ed_login).ends_with('='));
        let data_after_padding = format!("{}AAAA", b64(&ed_login));
        let key_after_padding = format!("{}{}", b64(&ed[..50]), b64(&ed[50..]));
        assert!(b64(&ed[..50]).ends_with('='));

        enum Want {
            Signed(u32),
            Refused(RefusalClass, &'static str, &'static str),
        }
        use RefusalClass::*;
        use Want::*;
        let invalid = |class| Refused(class, "invalid sign request", "INTERNAL_ERROR");
        let failed = |class| Refused(class, "sign operation failed", "SIGN_FAILED");
        let max = u64::from(u32::MAX);
        let cases: Vec<(&str, Envelope, Want)> = vec![
            (
                "ed25519 flags 0",
                with_flags(&ed, &ed_login, 0.into()),
                Signed(0),
            ),
            (
                "ed25519 flags 2",
                with_flags(&ed, &ed_login, 2.into()),
                Signed(2),
            ),
            (
                "ed25519 flags 4",
                with_flags(&ed, &ed_login, 4.into()),
                Signed(4),
            ),
            // Other key types pass the flags through untouched, every bit.
            (
                "ed25519 flags u32 max",
                with_flags(&ed, &ed_login, max.into()),
                Signed(u32::MAX),
            ),
            (
                "rsa-sha2-512 flags 0",
                with_flags(&rsa, &rsa_login, 0.into()),
                failed(UnparseableData),
            ),
            (
                "rsa-sha2-512 flags 2",
                with_flags(&rsa, &rsa_login, 2.into()),
                failed(UnparseableData),
            ),
            (
                "rsa-sha2-512 flags 4",
                with_flags(&rsa, &rsa_login, 4.into()),
                Signed(4),
            ),
            (
                "rsa-sha2-512 flags 6",
                with_flags(&rsa, &rsa_login, 6.into()),
                failed(UnparseableData),
            ),
            (
                "rsa-sha2-512 flags u32 max",
                with_flags(&rsa, &rsa_login, max.into()),
                failed(UnparseableData),
            ),
            (
                "flags one past u32 max",
                with_flags(&ed, &ed_login, (max + 1).into()),
                invalid(MalformedRequest),
            ),
            (
                "flags negative",
                with_flags(&ed, &ed_login, (-1).into()),
                invalid(MalformedRequest),
            ),
            (
                "flags a string",
                with_flags(&ed, &ed_login, "4".into()),
                invalid(MalformedRequest),
            ),
            (
                "flags a fraction",
                with_flags(&ed, &ed_login, 1.5.into()),
                invalid(MalformedRequest),
            ),
            (
                "flags null",
                with_flags(&ed, &ed_login, serde_json::Value::Null),
                invalid(MalformedRequest),
            ),
            (
                "flags missing",
                env(serde_json::json!({"public_key": b64(&ed), "data": b64(&ed_login)})),
                Signed(0),
            ),
            (
                "key missing",
                env(serde_json::json!({"data": b64(&ed_login), "flags": 0})),
                Refused(MalformedKey, "invalid public key", "KEY_NOT_FOUND"),
            ),
            (
                "key null",
                env(serde_json::json!({"public_key": null, "data": b64(&ed_login)})),
                invalid(MalformedRequest),
            ),
            (
                "data missing",
                env(serde_json::json!({"public_key": b64(&ed), "flags": 0})),
                failed(UnparseableData),
            ),
            (
                "data null",
                env(serde_json::json!({"public_key": b64(&ed), "data": null})),
                invalid(MalformedRequest),
            ),
            (
                "key base64 with more after the padding",
                env(serde_json::json!({"public_key": key_after_padding, "data": b64(&ed_login)})),
                Refused(
                    MalformedRequest,
                    "invalid public key encoding",
                    "INTERNAL_ERROR",
                ),
            ),
            (
                "data base64 with more after the padding",
                env(serde_json::json!({"public_key": b64(&ed), "data": data_after_padding})),
                Refused(
                    MalformedRequest,
                    "invalid challenge data encoding",
                    "INTERNAL_ERROR",
                ),
            ),
        ];
        for (name, env, want) in cases {
            let (gate, backend) = (CountingGate::new(true), RecordingBackend::signing());
            let (payload, entry) = run_sign(&env, &SignPolicy::default(), &gate, &backend).await;
            match want {
                Signed(flags) => {
                    assert_eq!(payload["format"], "ssh-ed25519", "{name}: {payload}");
                    assert_eq!(gate.calls(), 1, "{name}");
                    let calls = backend.calls();
                    assert_eq!(calls.len(), 1, "{name}");
                    assert_eq!(calls[0].2, flags, "{name}");
                    assert_eq!(entry.result, "ok", "{name}");
                    assert_eq!(entry.approval, "approve-all", "{name}");
                }
                Refused(class, error, code) => {
                    assert_eq!(
                        payload,
                        serde_json::json!({"error": error, "code": code}),
                        "{name}"
                    );
                    assert_eq!(gate.calls(), 0, "{name}: the gate was asked");
                    assert!(backend.calls().is_empty(), "{name}: the backend was asked");
                    assert_eq!(
                        (
                            entry.result.as_str(),
                            entry.code.as_str(),
                            entry.reason.as_str(),
                            entry.approval.as_str()
                        ),
                        ("refused", "SIGN_REFUSED", class.as_str(), "none"),
                        "{name}"
                    );
                    assert!(!entry.detail.is_empty(), "{name}: no detail");
                }
            }
        }
    }

    /// A declined request whose reply the shed server then refuses: the line becomes
    /// `undelivered`, keeps its code and detail, and its class stays at the front of
    /// the reason.
    #[tokio::test]
    async fn a_refusal_whose_reply_the_server_refuses_keeps_its_class_first() {
        let srv = ScriptedServer::spawn(
            vec![Answer(403, vec![], not_bound_body("desk"))],
            Answer(204, vec![], String::new()),
        )
        .await;
        let audit = Arc::new(CollectingAudit::default());
        let gate = CountingGate::new(true);
        let handlers = test_handlers(gate.clone(), audit.clone(), empty_backend());
        let key = fixed_ed25519_pub();
        let env = sign_env(
            &b64(&key),
            &b64(&login_as(b"root", &key, b"ssh-ed25519")),
            0,
        );
        handle_bus_message(
            &open_client(&srv.url()),
            "ssh-agent",
            &env,
            &never_shutdown(),
            &handlers,
        )
        .await;
        assert_eq!(gate.calls(), 0);
        assert_eq!(
            *audit.entries.lock().unwrap(),
            vec![AuditEntry {
                shed: "web".into(),
                ns: "ssh-agent".into(),
                op: "sign".into(),
                result: "undelivered".into(),
                detail: "login user=root".into(),
                code: "SIGN_REFUSED".into(),
                reason: "username_not_allowed; result refused, but the server refused the \
                         response (status 403 SOURCE_NOT_BOUND)"
                    .into(),
                approval: "none".into(),
                ..Default::default()
            }]
        );
    }

    /// A host-bound login recorded from a real OpenSSH client
    /// (`fixtures/sign_check/vectors.json`) is signed through the whole bus path:
    /// dispatch, the check, the gate once, the backend with exactly the recorded
    /// buffers and flags, the signature POSTed back, and an ok line.
    #[tokio::test]
    async fn a_recorded_host_bound_login_is_signed_through_the_bus() {
        let vectors: serde_json::Value =
            serde_json::from_str(include_str!("../fixtures/sign_check/vectors.json")).unwrap();
        let v = vectors["vectors"]
            .as_array()
            .unwrap()
            .iter()
            .find(|v| {
                v["shape"] == "login"
                    && v["expect"] == "accept"
                    && v["user_key"] == "ed25519"
                    && v["client"]
                        .as_str()
                        .unwrap()
                        .contains("PubkeyAuthentication=host-bound")
            })
            .expect("a recorded host-bound ed25519 login");
        let key = b64_decode(v["public_key_b64"].as_str().unwrap());
        let data = b64_decode(v["data_b64"].as_str().unwrap());
        let flags = v["flags"].as_u64().unwrap() as u32;
        // It really is the host-bound method.
        assert!(data
            .windows(b"publickey-hostbound-v00@openssh.com".len())
            .any(|w| w == b"publickey-hostbound-v00@openssh.com"));

        let server = MockServer::start_async().await;
        let respond = server
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/plugins/listeners/ssh-agent/respond")
                    .body_contains("\"in_reply_to\":\"sign-1\"")
                    .body_contains("\"format\":\"ssh-ed25519\"");
                t.status(204);
            })
            .await;
        let audit = Arc::new(CollectingAudit::default());
        let gate = CountingGate::new(true);
        let backend = RecordingBackend::signing();
        let handlers = test_handlers(gate.clone(), audit.clone(), backend.clone());
        let env = sign_env(
            v["public_key_b64"].as_str().unwrap(),
            v["data_b64"].as_str().unwrap(),
            flags,
        );
        handle_bus_message(
            &open_client(&server.base_url()),
            "ssh-agent",
            &env,
            &never_shutdown(),
            &handlers,
        )
        .await;
        respond.assert_async().await;
        assert_eq!(gate.calls(), 1);
        assert_eq!(backend.calls(), vec![(key, data, flags)]);
        let entries = audit.entries.lock().unwrap();
        assert_eq!(entries.len(), 1);
        assert_eq!(
            (
                entries[0].result.as_str(),
                entries[0].detail.as_str(),
                entries[0].reason.as_str()
            ),
            ("ok", "ssh-ed25519", "")
        );
    }

    fn b64_decode(s: &str) -> Vec<u8> {
        base64::engine::general_purpose::STANDARD.decode(s).unwrap()
    }

    #[tokio::test]
    async fn static_token_provider_open_mode_sends_nothing() {
        // A StaticTokenProvider("") behaves as open mode: bearer() → None.
        let client = BusClient::new(
            "http://x".to_string(),
            String::new(),
            Some(Arc::new(StaticTokenProvider::new(""))),
            None,
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
        let (payload, entry) =
            aws_dispatch(&aws_env(serde_json::json!({"operation":"ping"})), &aws, "").await;
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
        let (payload, entry) = aws_dispatch(
            &aws_env(serde_json::json!({"operation":"status"})),
            &aws,
            "",
        )
        .await;
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
        let (payload, _) = aws_dispatch(
            &aws_env(serde_json::json!({"operation":"status"})),
            &aws,
            "",
        )
        .await;
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
        let (payload, entry) = aws_dispatch(
            &aws_env(serde_json::json!({"operation": "delete"})),
            &aws,
            "",
        )
        .await;
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
            gate: deny_gate(), // ssh gate: deny-all
            audit: noop_audit(),
            backend: empty_backend(),
            sign_policy: Arc::new(SignPolicy::default()),
            refusal_audits: Arc::new(RefusalAuditLimiter::default()),
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
            &sign_env(&b64(&fixed_ed25519_pub()), &b64(&git_login()), 0),
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
    fn docker_handlers(
        backend: Arc<dyn DockerBackend>,
        gate: Arc<dyn ApprovalGate>,
    ) -> DockerHandlers {
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
        let (payload, entry) = docker_dispatch(
            &docker_get_env("ghcr.io"),
            &docker_handlers(backend, approve_gate()),
            "",
        )
        .await;
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
        let (payload, _entry) = docker_dispatch(
            &docker_get_env("blocked.io"),
            &docker_handlers(backend, approve_gate()),
            "",
        )
        .await;
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
        let (payload, entry) = docker_dispatch(
            &docker_get_env("ghcr.io"),
            &docker_handlers(dyn_backend, deny_gate()),
            "",
        )
        .await;
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
        let (payload, entry) = docker_dispatch(
            &docker_env(serde_json::json!({"operation":"ping"})),
            &docker_handlers(backend, approve_gate()),
            "",
        )
        .await;
        assert_eq!(payload, serde_json::json!({"status": "ok"}));
        assert!(entry.is_none(), "ping does not audit");
    }

    #[tokio::test]
    async fn docker_status_connected() {
        let backend: Arc<dyn DockerBackend> = FakeDockerBackend::with_status(true, 3);
        let (payload, entry) = docker_dispatch(
            &docker_env(serde_json::json!({"operation":"status"})),
            &docker_handlers(backend, approve_gate()),
            "",
        )
        .await;
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
        let (payload, entry) = docker_dispatch(
            &docker_env(serde_json::json!({"operation":"list"})),
            &docker_handlers(backend, approve_gate()),
            "",
        )
        .await;
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
        let (payload, entry) = docker_dispatch(
            &docker_env(serde_json::json!({"operation":"list"})),
            &docker_handlers(backend, approve_gate()),
            "",
        )
        .await;
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
        let (payload, entry) = docker_dispatch(
            &docker_env(serde_json::json!({})),
            &docker_handlers(dummy_docker(), approve_gate()),
            "",
        )
        .await;
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
            let (resp, entry) = docker_dispatch(
                &docker_env(payload),
                &docker_handlers(dummy_docker(), approve_gate()),
                "",
            )
            .await;
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
        let docker_backend: Arc<dyn DockerBackend> =
            FakeDockerBackend::get_ok(cred("ghcr.io", "u", "s"));
        let handlers = BusHandlers {
            gate: deny_gate(), // ssh gate: deny-all
            audit: noop_audit(),
            backend: empty_backend(),
            sign_policy: Arc::new(SignPolicy::default()),
            refusal_audits: Arc::new(RefusalAuditLimiter::default()),
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

    // ---- plan 035 §3.4: machine identity, the three refusals, respond retries ----

    const MID: &str = "0123456789abcdef0123456789abcdef";
    const OTHER_ID: &str = "fedcba9876543210fedcba9876543210";

    fn identity(name: &str) -> MachineIdentity {
        MachineIdentity {
            id: MID.into(),
            name: name.into(),
        }
    }

    fn not_bound_body(name: &str) -> String {
        serde_json::json!({
            "error": {"code": "SOURCE_NOT_BOUND", "message": "credential source for this server is \"desk\""},
            "source": {"machine_id": OTHER_ID, "machine_name": name},
        })
        .to_string()
    }

    fn api_error(code: &str) -> String {
        serde_json::json!({"error": {"code": code, "message": "refused"}}).to_string()
    }

    fn desk() -> ActiveSource {
        ActiveSource {
            machine_id: OTHER_ID.into(),
            machine_name: "desk".into(),
        }
    }

    /// The classification table. `not-source` iff 403 AND `error.code ==
    /// SOURCE_NOT_BOUND`; `duplicate` iff 409 AND `SOURCE_DUPLICATE_STREAM`; every row
    /// after the first two of each block is a negative control that must NOT take that
    /// state.
    #[test]
    fn classify_subscribe_refusal_table() {
        use SubscribeRefusal::*;
        let ns = |name: &str| {
            NotSource(ActiveSource {
                machine_id: OTHER_ID.into(),
                machine_name: name.into(),
            })
        };
        let cases: Vec<(u16, String, SubscribeRefusal)> = vec![
            // 403: not the source.
            (403, not_bound_body("desk"), ns("desk")),
            (403, not_bound_body("büro 2"), ns("büro 2")),
            // The code alone decides; a missing or malformed source names no one.
            (
                403,
                api_error("SOURCE_NOT_BOUND"),
                NotSource(ActiveSource::default()),
            ),
            (
                403,
                r#"{"error":{"code":"SOURCE_NOT_BOUND"},"source":{"machine_id":7,"machine_name":null}}"#.into(),
                NotSource(ActiveSource::default()),
            ),
            // 403 negatives: any other code, a released-broker refusal, a wrong case, the
            // code in the wrong place, and every unparseable body.
            (403, api_error("SOURCE_MACHINE_ID_REQUIRED"), Other),
            (403, api_error("FORBIDDEN"), Other),
            (403, api_error("NAMESPACE_RESERVED"), Other),
            (403, api_error("source_not_bound"), Other),
            (403, r#"{"code":"SOURCE_NOT_BOUND"}"#.into(), Other),
            (403, r#"{"error":"SOURCE_NOT_BOUND"}"#.into(), Other),
            (403, r#"{"error":{"code":["SOURCE_NOT_BOUND"]}}"#.into(), Other),
            (403, "SOURCE_NOT_BOUND".into(), Other),
            (403, r#"{"error":{"code":"SOURCE_NOT_BOUND""#.into(), Other),
            (403, r#"["SOURCE_NOT_BOUND"]"#.into(), Other),
            (403, String::new(), Other),
            // The right code under the wrong status is not not-source either.
            (503, not_bound_body("desk"), Other),
            (400, not_bound_body("desk"), Other),
            (409, not_bound_body("desk"), Conflict),
            (401, not_bound_body("desk"), Unauthorized),
            // 409: the duplicate is retryable; every other 409 is terminal as before.
            (409, api_error("SOURCE_DUPLICATE_STREAM"), Duplicate),
            (409, api_error("NAMESPACE_ALREADY_REGISTERED"), Conflict),
            (409, String::new(), Conflict),
            (409, "namespace \"ssh-agent\" is already registered".into(), Conflict),
            (409, api_error("source_duplicate_stream"), Conflict),
            (403, api_error("SOURCE_DUPLICATE_STREAM"), Other),
            // 401 keeps its meaning whatever the body says.
            (401, String::new(), Unauthorized),
            (401, api_error("SOURCE_DUPLICATE_STREAM"), Unauthorized),
            (500, String::new(), Other),
        ];
        for (status, body, want) in cases {
            assert_eq!(
                classify_subscribe_refusal(status, &body, true),
                want,
                "status {status} body {body:?}"
            );
        }
    }

    /// A body that was cut short is never a pin refusal, even when the part read parses
    /// on its own — a complete `SOURCE_NOT_BOUND` (or `SOURCE_DUPLICATE_STREAM`) object,
    /// whitespace padding up to the byte cap, then garbage past it.
    #[test]
    fn an_incomplete_body_is_never_a_pin_refusal() {
        for (status, code, cut) in [
            (403u16, CODE_SOURCE_NOT_BOUND, SubscribeRefusal::Other),
            (
                409,
                CODE_SOURCE_DUPLICATE_STREAM,
                SubscribeRefusal::Conflict,
            ),
        ] {
            let body = api_error(code);
            assert_ne!(
                classify_subscribe_refusal(status, &body, true),
                cut,
                "control: the whole body classifies"
            );
            assert_eq!(
                classify_subscribe_refusal(status, &body, false),
                cut,
                "{status}"
            );
        }
        assert_eq!(
            classify_subscribe_refusal(401, "", false),
            SubscribeRefusal::Unauthorized
        );
    }

    /// The server names the active machine, so its strings are cleaned before they
    /// reach a log or a status line: no control characters, bounded length.
    #[test]
    fn active_source_strings_are_cleaned() {
        let long = "x".repeat(500);
        let body = serde_json::json!({
            "error": {"code": "SOURCE_NOT_BOUND"},
            "source": {"machine_id": long, "machine_name": "desk\n\u{1b}[31mred\u{7}"},
        })
        .to_string();
        let SubscribeRefusal::NotSource(src) = classify_subscribe_refusal(403, &body, true) else {
            panic!("not classified as not-source");
        };
        assert_eq!(src.machine_name, "desk[31mred");
        assert_eq!(src.machine_id.len(), MAX_SOURCE_FIELD_BYTES);
        // A multi-byte character is never cut in half.
        assert_eq!(display_field("ééé", 5), "éé");
    }

    #[test]
    fn percent_encode_name_table() {
        for (name, want) in [
            ("laptop", "laptop"),
            ("Mini-2.lan_x~", "Mini-2.lan_x~"),
            ("laptop (work)", "laptop%20%28work%29"),
            ("a+b%c/d", "a%2Bb%25c%2Fd"),
            ("büro", "b%C3%BCro"),
            ("日本", "%E6%97%A5%E6%9C%AC"),
        ] {
            assert_eq!(percent_encode_name(name), want, "{name:?}");
        }
    }

    /// An identity this build would not write is never put on the wire.
    #[test]
    fn with_machine_drops_an_invalid_identity() {
        let c = open_client("http://x").with_machine(Some(identity("ok")));
        assert!(c.machine.is_some());
        for bad in [
            MachineIdentity {
                id: "0123".into(),
                name: "ok".into(),
            },
            MachineIdentity {
                id: MID.to_uppercase(),
                name: "ok".into(),
            },
            MachineIdentity {
                id: MID.into(),
                name: "tab\there".into(),
            },
            MachineIdentity {
                id: MID.into(),
                name: String::new(),
            },
        ] {
            let c = open_client("http://x").with_machine(Some(bad.clone()));
            assert!(c.machine.is_none(), "{bad:?} must not be sent");
        }
    }

    /// A tiny HTTP/1.1 server answering each request from a script (then a default),
    /// recording every request's method, path and headers. Each answer closes the
    /// connection, so every request is one fresh accept.
    struct ScriptedServer {
        addr: std::net::SocketAddr,
        seen: Arc<Mutex<Vec<SeenRequest>>>,
        notify: Arc<tokio::sync::Notify>,
    }

    #[derive(Clone, Debug)]
    struct SeenRequest {
        method: String,
        path: String,
        headers: Vec<(String, String)>,
        at: Instant,
    }

    impl SeenRequest {
        fn header(&self, name: &str) -> Option<String> {
            self.headers
                .iter()
                .find(|(k, _)| k.eq_ignore_ascii_case(name))
                .map(|(_, v)| v.clone())
        }
    }

    /// One scripted answer: status, extra headers, body.
    #[derive(Clone)]
    struct Answer(u16, Vec<(&'static str, String)>, String);

    /// A scripted reply: a plain [`Answer`], or one that misbehaves in time.
    #[derive(Clone)]
    enum Reply {
        Plain(Answer),
        /// The head (with `Content-Length: declared`) and `first` at once, then after
        /// `pause` the `rest` (or, with `None`, nothing more: the body never ends).
        Slow {
            status: u16,
            headers: Vec<(&'static str, String)>,
            declared: usize,
            first: String,
            pause: Duration,
            rest: Option<String>,
        },
        /// Read the request and never answer it.
        Silent,
        /// Read the request, write nothing for `after`, then answer with `answer` — a
        /// server holding an answer behind its delivery deadline.
        Delayed {
            after: Duration,
            answer: Answer,
        },
    }

    impl From<Answer> for Reply {
        fn from(a: Answer) -> Reply {
            Reply::Plain(a)
        }
    }

    impl ScriptedServer {
        async fn spawn(script: Vec<Answer>, default: Answer) -> ScriptedServer {
            Self::spawn_replies(
                script.into_iter().map(Reply::from).collect(),
                default.into(),
            )
            .await
        }

        async fn spawn_replies(script: Vec<Reply>, default: Reply) -> ScriptedServer {
            use tokio::io::{AsyncReadExt, AsyncWriteExt};
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
            let addr = listener.local_addr().unwrap();
            let seen: Arc<Mutex<Vec<SeenRequest>>> = Arc::default();
            let notify = Arc::new(tokio::sync::Notify::new());
            let script = Arc::new(Mutex::new(std::collections::VecDeque::from(script)));
            let (seen2, notify2) = (seen.clone(), notify.clone());
            tokio::spawn(async move {
                while let Ok((mut sock, _)) = listener.accept().await {
                    let mut buf = Vec::new();
                    let mut tmp = [0u8; 4096];
                    // Read the head, then any Content-Length body.
                    let head_end = loop {
                        let n = match sock.read(&mut tmp).await {
                            Ok(0) | Err(_) => break None,
                            Ok(n) => n,
                        };
                        buf.extend_from_slice(&tmp[..n]);
                        if let Some(i) = buf.windows(4).position(|w| w == b"\r\n\r\n") {
                            break Some(i);
                        }
                    };
                    let Some(head_end) = head_end else { continue };
                    let head = String::from_utf8_lossy(&buf[..head_end]).to_string();
                    let mut lines = head.split("\r\n");
                    let mut first = lines.next().unwrap_or("").split(' ');
                    let method = first.next().unwrap_or("").to_string();
                    let path = first.next().unwrap_or("").to_string();
                    let headers: Vec<(String, String)> = lines
                        .filter_map(|l| l.split_once(':'))
                        .map(|(k, v)| (k.trim().to_string(), v.trim().to_string()))
                        .collect();
                    let len: usize = headers
                        .iter()
                        .find(|(k, _)| k.eq_ignore_ascii_case("content-length"))
                        .and_then(|(_, v)| v.parse().ok())
                        .unwrap_or(0);
                    while buf.len() < head_end + 4 + len {
                        match sock.read(&mut tmp).await {
                            Ok(0) | Err(_) => break,
                            Ok(n) => buf.extend_from_slice(&tmp[..n]),
                        }
                    }
                    seen2.lock().unwrap().push(SeenRequest {
                        method,
                        path,
                        headers,
                        at: Instant::now(),
                    });
                    notify2.notify_waiters();
                    let reply = script
                        .lock()
                        .unwrap()
                        .pop_front()
                        .unwrap_or_else(|| default.clone());
                    let head = |status: u16, len: usize, extra: &[(&str, String)]| {
                        let mut h = format!(
                            "HTTP/1.1 {status} X\r\nContent-Length: {len}\r\nConnection: close\r\n"
                        );
                        for (k, v) in extra {
                            h.push_str(&format!("{k}: {v}\r\n"));
                        }
                        h.push_str("\r\n");
                        h
                    };
                    // Each reply is written on its own task, so a slow or silent one never
                    // holds up the next accept.
                    match reply {
                        Reply::Plain(Answer(status, extra, body)) => {
                            let resp = head(status, body.len(), &extra) + &body;
                            tokio::spawn(async move {
                                let _ = sock.write_all(resp.as_bytes()).await;
                                let _ = sock.shutdown().await;
                            });
                        }
                        Reply::Slow {
                            status,
                            headers,
                            declared,
                            first,
                            pause,
                            rest,
                        } => {
                            let start = head(status, declared, &headers) + &first;
                            tokio::spawn(async move {
                                let _ = sock.write_all(start.as_bytes()).await;
                                let _ = sock.flush().await;
                                tokio::time::sleep(pause).await;
                                match rest {
                                    Some(rest) => {
                                        let _ = sock.write_all(rest.as_bytes()).await;
                                        let _ = sock.shutdown().await;
                                    }
                                    None => std::future::pending::<()>().await,
                                }
                            });
                        }
                        Reply::Silent => {
                            tokio::spawn(async move {
                                let _keep = sock;
                                std::future::pending::<()>().await;
                            });
                        }
                        Reply::Delayed {
                            after,
                            answer: Answer(status, extra, body),
                        } => {
                            let resp = head(status, body.len(), &extra) + &body;
                            tokio::spawn(async move {
                                tokio::time::sleep(after).await;
                                let _ = sock.write_all(resp.as_bytes()).await;
                                let _ = sock.shutdown().await;
                            });
                        }
                    }
                }
            });
            ScriptedServer { addr, seen, notify }
        }

        fn url(&self) -> String {
            format!("http://{}", self.addr)
        }

        fn seen(&self) -> Vec<SeenRequest> {
            self.seen.lock().unwrap().clone()
        }

        /// Wait (bounded) until at least `n` requests have arrived.
        async fn wait_for(&self, n: usize, within: Duration) -> Vec<SeenRequest> {
            let deadline = Instant::now() + within;
            loop {
                let notified = self.notify.notified();
                let seen = self.seen();
                if seen.len() >= n {
                    return seen;
                }
                let left = deadline.saturating_duration_since(Instant::now());
                assert!(
                    !left.is_zero(),
                    "only {} of {n} requests arrived",
                    seen.len()
                );
                let _ = tokio::time::timeout(left.min(Duration::from_millis(50)), notified).await;
            }
        }
    }

    fn sse_one(id: &str) -> String {
        format!(
            "data: {{\"id\":\"{id}\",\"namespace\":\"ssh-agent\",\"type\":\"request\",\"final\":true,\"timestamp\":\"t\",\"payload\":{{\"operation\":\"ping\"}}}}\n\n"
        )
    }

    async fn wait_state(sub: &Subscription, want: ConnState, within: Duration) -> SubStatus {
        let deadline = Instant::now() + within;
        loop {
            let s = sub.status();
            if s.state == want {
                return s;
            }
            assert!(
                Instant::now() < deadline,
                "never reached {want:?}; last {s:?}"
            );
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    }

    fn fast(c: BusClient) -> BusClient {
        c.with_test_backoff(Duration::from_millis(5), Duration::from_millis(20))
    }

    fn provider_client(url: &str, tp: Arc<SeqTokenProvider>, log: Arc<dyn BusLog>) -> BusClient {
        BusClient::new(url.to_string(), String::new(), Some(tp), None, None, log).unwrap()
    }

    /// Subscribe and respond both carry the id and the percent-encoded name.
    #[tokio::test]
    async fn subscribe_and_respond_carry_the_identity_headers() {
        let srv = ScriptedServer::spawn(
            vec![Answer(
                200,
                vec![("Content-Type", "text/event-stream".into())],
                sse_one("e1"),
            )],
            Answer(204, vec![], String::new()),
        )
        .await;
        let client = fast(open_client(&srv.url())).with_machine(Some(identity("laptop (work)")));
        let (tx, rx) = shutdown_pair();
        let mut sub = client.subscribe("ssh-agent", rx);
        tokio::time::timeout(Duration::from_secs(5), sub.rx.recv())
            .await
            .expect("an envelope")
            .unwrap();
        let _ = tx.send(true);
        let env = Envelope::new_response("e1", "ssh-agent", serde_json::Value::Null);
        client
            .respond("ssh-agent", &env, &never_shutdown())
            .await
            .unwrap();
        let seen = srv.seen();
        let get = seen
            .iter()
            .find(|r| r.method == "GET")
            .expect("a subscribe");
        let post = seen.iter().find(|r| r.method == "POST").expect("a respond");
        assert_eq!(get.path, "/api/plugins/listeners/ssh-agent/messages");
        assert_eq!(post.path, "/api/plugins/listeners/ssh-agent/respond");
        for r in [get, post] {
            assert_eq!(r.header(HEADER_MACHINE_ID).as_deref(), Some(MID), "{r:?}");
            assert_eq!(
                r.header(HEADER_MACHINE_NAME).as_deref(),
                Some("laptop%20%28work%29"),
                "{r:?}"
            );
        }
    }

    /// No identity: neither header, on either request — the wire is as before.
    #[tokio::test]
    async fn no_identity_sends_no_identity_headers() {
        let srv = ScriptedServer::spawn(
            vec![Answer(200, vec![], sse_one("e1"))],
            Answer(204, vec![], String::new()),
        )
        .await;
        let client = fast(open_client(&srv.url())).with_machine(None);
        let (tx, rx) = shutdown_pair();
        let mut sub = client.subscribe("ssh-agent", rx);
        tokio::time::timeout(Duration::from_secs(5), sub.rx.recv())
            .await
            .unwrap()
            .unwrap();
        let _ = tx.send(true);
        let env = Envelope::new_response("e1", "ssh-agent", serde_json::Value::Null);
        client
            .respond("ssh-agent", &env, &never_shutdown())
            .await
            .unwrap();
        for r in srv.seen() {
            assert_eq!(r.header(HEADER_MACHINE_ID), None, "{r:?}");
            assert_eq!(r.header(HEADER_MACHINE_NAME), None, "{r:?}");
        }
    }

    /// Not the source: the state names the active machine, the loop keeps retrying on
    /// its backoff WITHOUT re-minting, says so once at INFO, and connects on its own once
    /// the server accepts it.
    #[tokio::test]
    async fn not_source_retries_without_reminting_then_connects() {
        let refusal = Answer(403, vec![], not_bound_body("desk"));
        let srv = ScriptedServer::spawn(
            vec![refusal.clone(), refusal.clone(), refusal.clone(), refusal],
            Answer(200, vec![], sse_one("after")),
        )
        .await;
        let tp = SeqTokenProvider::new();
        let log = Arc::new(CountingLog::default());
        let client = fast(provider_client(&srv.url(), tp.clone(), log.clone()))
            .with_machine(Some(identity("laptop")));
        let (tx, rx) = shutdown_pair();
        let mut sub = client.subscribe("ssh-agent", rx);
        let s = wait_state(&sub, ConnState::NotSource, Duration::from_secs(5)).await;
        assert_eq!(s.active_source, Some(desk()));
        assert!(
            s.last_error.contains("the active source is desk"),
            "{}",
            s.last_error
        );
        let e = tokio::time::timeout(Duration::from_secs(5), sub.rx.recv())
            .await
            .expect("connects once the server accepts it")
            .unwrap();
        assert_eq!(e.id, "after");
        let _ = tx.send(true);
        assert!(srv.seen().len() >= 5, "four refusals then the accept");
        assert_eq!(
            tp.invalidated(),
            0,
            "a not-source refusal must never re-mint"
        );
        let infos: Vec<String> = log
            .lines()
            .into_iter()
            .filter(|l| l.starts_with("INFO ") && l.contains("not the credential source"))
            .collect();
        assert_eq!(infos.len(), 1, "said once at INFO, then quiet: {infos:?}");
        // Never louder than INFO (the scripted server closing the accepted stream
        // afterwards is a WARN of its own, about the drop).
        assert!(
            !log.lines()
                .iter()
                .any(|l| (l.starts_with("WARN ") || l.starts_with("ERROR "))
                    && l.contains("not the credential source")),
            "{:?}",
            log.lines()
        );
        // Leaving the state clears the active source.
        let after = sub.status();
        assert_ne!(after.state, ConnState::NotSource);
        assert_eq!(after.active_source, None);
    }

    /// A duplicate stream (this machine's id already holds the namespace in another
    /// process) keeps retrying, without re-minting; it is never terminal.
    #[tokio::test]
    async fn duplicate_stream_keeps_retrying() {
        let srv = ScriptedServer::spawn(
            vec![],
            Answer(409, vec![], api_error(CODE_SOURCE_DUPLICATE_STREAM)),
        )
        .await;
        let tp = SeqTokenProvider::new();
        let client = fast(provider_client(&srv.url(), tp.clone(), silent_log()))
            .with_machine(Some(identity("laptop")));
        let (tx, rx) = shutdown_pair();
        let sub = client.subscribe("ssh-agent", rx);
        srv.wait_for(3, Duration::from_secs(5)).await;
        let s = sub.status();
        assert_eq!(s.state, ConnState::Duplicate);
        assert!(
            s.last_error.contains("duplicate stream"),
            "{}",
            s.last_error
        );
        assert_eq!(s.active_source, None);
        assert_eq!(tp.invalidated(), 0);
        let _ = tx.send(true);
    }

    /// Negative controls on the live loop: a 403 with another code, and a 403 whose body
    /// does not parse, are plain `reconnecting` with the server's body shown — never
    /// not-source — and keep retrying.
    #[tokio::test]
    async fn other_403s_are_reconnecting_never_not_source() {
        for body in [
            api_error("FORBIDDEN"),
            api_error("SOURCE_MACHINE_ID_REQUIRED"),
            "SOURCE_NOT_BOUND but not json".to_string(),
        ] {
            let srv = ScriptedServer::spawn(vec![], Answer(403, vec![], body.clone())).await;
            let client = fast(open_client(&srv.url())).with_machine(Some(identity("laptop")));
            let (tx, rx) = shutdown_pair();
            let sub = client.subscribe("ssh-agent", rx);
            srv.wait_for(3, Duration::from_secs(5)).await;
            let s = sub.status();
            assert_eq!(s.state, ConnState::Reconnecting, "{body}");
            assert_eq!(s.last_error, format!("unexpected status 403: {body}"));
            assert_eq!(s.active_source, None);
            let _ = tx.send(true);
        }
    }

    /// A 409 with the API's ordinary already-registered body stays terminal: one hit.
    #[tokio::test]
    async fn api_shaped_409_without_the_duplicate_code_is_terminal() {
        let srv = ScriptedServer::spawn(
            vec![],
            Answer(409, vec![], api_error("NAMESPACE_ALREADY_REGISTERED")),
        )
        .await;
        let client = fast(open_client(&srv.url())).with_machine(Some(identity("laptop")));
        let (_tx, rx) = shutdown_pair();
        let mut sub = client.subscribe("ssh-agent", rx);
        let closed = tokio::time::timeout(Duration::from_secs(3), sub.rx.recv())
            .await
            .expect("terminal: the channel closes");
        assert!(closed.is_none());
        assert_eq!(sub.status().state, ConnState::Rejected);
        tokio::time::sleep(Duration::from_millis(100)).await;
        assert_eq!(srv.seen().len(), 1, "no retry after a terminal 409");
    }

    /// `retry_now` cuts a long backoff short: with a 10 s floor, the second attempt
    /// comes at once after the wake rather than ten seconds later.
    #[tokio::test]
    async fn retry_now_cuts_the_backoff_short() {
        let srv = ScriptedServer::spawn(vec![], Answer(403, vec![], not_bound_body("desk"))).await;
        let client = open_client(&srv.url())
            .with_test_backoff(Duration::from_secs(10), Duration::from_secs(10))
            .with_machine(Some(identity("laptop")));
        let (tx, rx) = shutdown_pair();
        let sub = client.subscribe("ssh-agent", rx);
        wait_state(&sub, ConnState::NotSource, Duration::from_secs(5)).await;
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert_eq!(srv.seen().len(), 1, "still backing off");
        client.retry_now();
        srv.wait_for(2, Duration::from_secs(3)).await;
        let _ = tx.send(true);
    }

    fn retry_after(status: u16, secs: &str) -> Answer {
        Answer(
            status,
            vec![("Retry-After", secs.to_string())],
            api_error("BUSY"),
        )
    }

    /// A 503 or 429 that carries `Retry-After` is re-sent, and the answer delivered.
    #[tokio::test]
    async fn respond_retries_503_and_429_with_retry_after() {
        for status in [503u16, 429] {
            let srv = ScriptedServer::spawn(
                vec![retry_after(status, "0"), retry_after(status, "0")],
                Answer(204, vec![], String::new()),
            )
            .await;
            let client = open_client(&srv.url()).with_machine(Some(identity("laptop")));
            let env = Envelope::new_response("r", "ns", serde_json::Value::Null);
            client
                .respond("ns", &env, &never_shutdown())
                .await
                .unwrap_or_else(|e| panic!("{status}: {e}"));
            let seen = srv.seen();
            assert_eq!(seen.len(), 3, "{status}: two refusals then the 204");
            // Every attempt carries the identity, and a `Retry-After: 0` still waits.
            for r in &seen {
                assert_eq!(r.header(HEADER_MACHINE_ID).as_deref(), Some(MID));
            }
            assert!(seen[1].at - seen[0].at >= RESPOND_RETRY_MIN_WAIT);
        }
    }

    /// Never retried: a 503/429 without `Retry-After`, a 403, a 413 — one send each,
    /// the status surfaced.
    #[tokio::test]
    async fn respond_does_not_retry_other_refusals() {
        for answer in [
            Answer(503, vec![], api_error("PLUGINS_DISABLED")),
            Answer(429, vec![], String::new()),
            Answer(
                403,
                vec![("Retry-After", "0".into())],
                api_error("FORBIDDEN"),
            ),
            Answer(
                413,
                vec![("Retry-After", "0".into())],
                api_error("REQUEST_TOO_LARGE"),
            ),
            Answer(500, vec![("Retry-After", "0".into())], String::new()),
        ] {
            let status = answer.0;
            let srv = ScriptedServer::spawn(vec![answer], Answer(204, vec![], String::new())).await;
            let env = Envelope::new_response("r", "ns", serde_json::Value::Null);
            let err = open_client(&srv.url())
                .respond("ns", &env, &never_shutdown())
                .await
                .unwrap_err();
            assert!(
                matches!(err, BusError::BadStatus(s, _) if s == status),
                "{status}: {err:?}"
            );
            assert_eq!(srv.seen().len(), 1, "{status} must not be retried");
        }
    }

    /// The retry is bounded: a server that keeps answering 503 gets the first send plus
    /// `RESPOND_MAX_RETRIES`, and a `Retry-After` past the budget is not waited for.
    #[tokio::test]
    async fn respond_retry_is_bounded() {
        let srv = ScriptedServer::spawn(vec![], retry_after(503, "0")).await;
        let env = Envelope::new_response("r", "ns", serde_json::Value::Null);
        let err = open_client(&srv.url())
            .respond("ns", &env, &never_shutdown())
            .await
            .unwrap_err();
        assert!(matches!(err, BusError::BadStatus(503, _)), "{err:?}");
        assert_eq!(srv.seen().len(), 1 + RESPOND_MAX_RETRIES as usize);

        let srv = ScriptedServer::spawn(vec![], retry_after(429, "60")).await;
        let started = Instant::now();
        let err = open_client(&srv.url())
            .respond("ns", &env, &never_shutdown())
            .await
            .unwrap_err();
        assert!(matches!(err, BusError::BadStatus(429, _)), "{err:?}");
        assert_eq!(srv.seen().len(), 1, "a wait past the budget is not taken");
        assert!(started.elapsed() < Duration::from_secs(2));
    }

    #[test]
    fn respond_retry_wait_table() {
        use reqwest::header::HeaderValue;
        let v = |s: &str| HeaderValue::from_str(s).unwrap();
        // A five-second remainder, so the table reads the same whatever the budget.
        let budget = Duration::from_secs(5);
        // No attempt length here: the wait alone against the remainder (the whole
        // attempt that must also fit is `respond_retry_needs_room_for_a_whole_attempt`).
        let ok = |status: u16, raw: &str, remaining: Duration| {
            respond_retry_wait(status, Some(&v(raw)), 0, remaining, Duration::ZERO)
        };
        assert_eq!(ok(503, "2", budget), Some(Duration::from_secs(2)));
        assert_eq!(ok(429, "0", budget), Some(RESPOND_RETRY_MIN_WAIT));
        assert_eq!(ok(503, " 1 ", budget), Some(Duration::from_secs(1)));
        // Only a non-negative whole number of seconds that fits what is left of the
        // deadline: everything else means do not retry (never "one second").
        for raw in [
            "Wed, 21 Oct 2015 07:28:00 GMT",
            "Fri, 01 Jan 2100 00:00:00 GMT",
            "18446744073709551615",
            "18446744073709551616",
            "99999999999999999999999999",
            "-1",
            "+1",
            "1.5",
            "1, 2",
            "1 2",
            "soon",
            "",
            "5",
            "60",
        ] {
            assert_eq!(ok(503, raw, budget), None, "Retry-After {raw:?}");
        }
        // The wait must end inside the remaining budget, not the whole one.
        assert_eq!(
            ok(503, "2", Duration::from_secs(3)),
            Some(Duration::from_secs(2))
        );
        assert_eq!(ok(503, "2", Duration::from_secs(2)), None);
        assert_eq!(ok(503, "0", RESPOND_RETRY_MIN_WAIT), None);
        assert_eq!(ok(503, "0", Duration::ZERO), None);
        // Negative controls.
        assert_eq!(
            respond_retry_wait(503, None, 0, budget, Duration::ZERO),
            None
        );
        assert_eq!(ok(403, "0", budget), None);
        assert_eq!(ok(413, "0", budget), None);
        assert_eq!(ok(500, "0", budget), None);
        assert_eq!(
            respond_retry_wait(
                503,
                Some(&v("0")),
                RESPOND_MAX_RETRIES,
                budget,
                Duration::ZERO
            ),
            None
        );
    }

    /// A refusal body padded past the byte cap is cut short, and a cut-short body is
    /// never a pin refusal on the live loop either: a complete `SOURCE_NOT_BOUND` object,
    /// whitespace to the cap, then garbage is plain `reconnecting`; the same with
    /// `SOURCE_DUPLICATE_STREAM` under a 409 is terminal, never retried.
    #[tokio::test]
    async fn a_padded_refusal_body_is_never_a_pin_state() {
        let padded = |json: String| {
            let pad = MAX_ERROR_BODY_BYTES - json.len();
            format!("{json}{}garbage that is not JSON", " ".repeat(pad))
        };
        let srv =
            ScriptedServer::spawn(vec![], Answer(403, vec![], padded(not_bound_body("desk"))))
                .await;
        let client = fast(open_client(&srv.url())).with_machine(Some(identity("laptop")));
        let (tx, rx) = shutdown_pair();
        let sub = client.subscribe("ssh-agent", rx);
        srv.wait_for(3, Duration::from_secs(5)).await;
        let s = sub.status();
        assert_eq!(s.state, ConnState::Reconnecting, "{s:?}");
        assert_eq!(s.active_source, None);
        let _ = tx.send(true);

        let srv = ScriptedServer::spawn(
            vec![],
            Answer(409, vec![], padded(api_error(CODE_SOURCE_DUPLICATE_STREAM))),
        )
        .await;
        let client = fast(open_client(&srv.url())).with_machine(Some(identity("laptop")));
        let (_tx, rx) = shutdown_pair();
        let mut sub = client.subscribe("ssh-agent", rx);
        let closed = tokio::time::timeout(Duration::from_secs(3), sub.rx.recv())
            .await
            .expect("terminal: the channel closes");
        assert!(closed.is_none());
        assert_eq!(sub.status().state, ConnState::Rejected);
        assert_eq!(srv.seen().len(), 1, "a terminal 409 is not retried");
    }

    /// A refusal body that never ends does not stall the subscribe loop: the read gives
    /// up after [`ERROR_BODY_READ_TIMEOUT`], the cut-short body is not classified (a
    /// `SOURCE_NOT_BOUND` prefix stays plain `reconnecting`), and the loop retries.
    #[tokio::test]
    async fn a_never_ending_refusal_body_does_not_stall_the_subscribe_loop() {
        let body = not_bound_body("desk");
        let srv = ScriptedServer::spawn_replies(
            vec![],
            Reply::Slow {
                status: 403,
                headers: vec![],
                declared: body.len() + 10,
                first: body,
                pause: Duration::ZERO,
                rest: None,
            },
        )
        .await;
        let client = fast(open_client(&srv.url())).with_machine(Some(identity("laptop")));
        let (tx, rx) = shutdown_pair();
        let sub = client.subscribe("ssh-agent", rx);
        let seen = srv
            .wait_for(2, ERROR_BODY_READ_TIMEOUT + Duration::from_secs(3))
            .await;
        let gap = seen[1].at - seen[0].at;
        assert!(
            gap >= ERROR_BODY_READ_TIMEOUT,
            "the read waited its bound: {gap:?}"
        );
        let s = sub.status();
        assert_eq!(s.state, ConnState::Reconnecting, "{s:?}");
        assert!(s.last_error.starts_with("unexpected status 403: "), "{s:?}");
        assert_eq!(s.active_source, None);
        let _ = tx.send(true);
    }

    /// Server text shown to a person — the active machine's name and id, and refusal
    /// bodies in status and in the log — loses its controls AND its invisible format
    /// characters (bidirectional overrides and isolates, zero-width characters, the BOM),
    /// so a server can neither forge a line, send a terminal escape, nor disguise a name.
    #[tokio::test]
    async fn server_text_is_cleaned_before_it_is_shown_or_logged() {
        let hostile =
            "de\u{202E}ks\u{200B}\u{2066}x\u{2069}\u{FEFF}\u{061C}\u{200F}\u{2060}\u{00AD}!";
        assert_eq!(display_field(hostile, 64), "deksx!");
        let body = serde_json::json!({
            "error": {"code": "SOURCE_NOT_BOUND"},
            "source": {"machine_id": "ab\u{202A}cd", "machine_name": hostile},
        })
        .to_string();
        let SubscribeRefusal::NotSource(src) = classify_subscribe_refusal(403, &body, true) else {
            panic!("not classified as not-source");
        };
        assert_eq!(src.machine_name, "deksx!");
        assert_eq!(src.machine_id, "abcd");

        // A plain 403 body with a forged log line, a terminal escape and a bidi override,
        // on the subscribe loop (status) and on a respond (the warning log).
        let raw = "denied\n2026-01-01 INFO forged line\u{1b}[2J\u{202E}tail\u{7}";
        let srv = ScriptedServer::spawn(vec![], Answer(403, vec![], raw.into())).await;
        let log = Arc::new(CountingLog::default());
        let client = fast(open_client_with(&srv.url(), log.clone()));
        let (tx, rx) = shutdown_pair();
        let sub = client.subscribe("ssh-agent", rx);
        srv.wait_for(2, Duration::from_secs(5)).await;
        let want = "denied2026-01-01 INFO forged line[2Jtail";
        assert_eq!(
            sub.status().last_error,
            format!("unexpected status 403: {want}")
        );
        let _ = tx.send(true);

        let audit = Arc::new(CollectingAudit::default());
        let handlers = test_handlers(approve_gate(), audit.clone(), empty_backend());
        handle_bus_message(
            &client,
            "ssh-agent",
            &list_request(),
            &never_shutdown(),
            &handlers,
        )
        .await;
        let lines = log.lines();
        let warn = lines
            .iter()
            .find(|l| l.starts_with("WARN ") && l.contains("failed to send"))
            .expect("the failed respond is logged");
        assert!(warn.ends_with(want), "{warn:?}");
        for l in &lines {
            assert!(
                !l.chars().any(|c| c.is_control() || is_format_char(c)),
                "{l:?}"
            );
        }
    }

    fn list_request() -> Envelope {
        Envelope {
            id: "req-list".into(),
            namespace: "ssh-agent".into(),
            msg_type: "request".into(),
            in_reply_to: String::new(),
            is_final: true,
            timestamp: "t".into(),
            payload: Some(serde_json::json!({"operation": "list"})),
            shed: Some(ShedInfo {
                name: "web".into(),
                backend: "vz".into(),
                server: "s".into(),
            }),
        }
    }

    /// The five-second respond budget the deadline cells run on, with a two-second
    /// per-POST wait (so a retry has room for a whole attempt), so they stay quick; the
    /// real numbers are pinned by `respond_timing_outlasts_the_servers_delivery_deadline`.
    const SHORT_BUDGET: Duration = Duration::from_secs(5);
    const SHORT_ATTEMPT: Duration = Duration::from_secs(2);

    fn short_respond(c: BusClient) -> BusClient {
        c.with_test_respond_timing(SHORT_ATTEMPT, SHORT_BUDGET)
    }

    /// One deadline from the first send covers every refusal body read: a server that
    /// answers `503 Retry-After: 0` at once but dribbles each body out over four seconds
    /// cannot stretch the respond past its budget, and no POST starts after it. (These
    /// deadline cells run on a five-second budget, [`short_respond`].)
    #[tokio::test]
    async fn respond_deadline_covers_slow_refusal_bodies() {
        let body = api_error("BUSY");
        let slow = Reply::Slow {
            status: 503,
            headers: vec![("Retry-After", "0".into())],
            declared: body.len(),
            first: String::new(),
            pause: Duration::from_secs(4),
            rest: Some(body),
        };
        let srv = ScriptedServer::spawn_replies(vec![], slow).await;
        let env = Envelope::new_response("r", "ns", serde_json::Value::Null);
        let started = Instant::now();
        let r = tokio::time::timeout(
            Duration::from_secs(20),
            short_respond(open_client(&srv.url())).respond("ns", &env, &never_shutdown()),
        )
        .await
        .expect("respond returns");
        let took = started.elapsed();
        assert!(matches!(r, Err(BusError::BadStatus(503, _))), "{r:?}");
        assert!(
            took < SHORT_BUDGET + Duration::from_millis(900),
            "the deadline covers the body reads: {took:?}"
        );
        let first = srv.seen()[0].at;
        for (i, req) in srv.seen().iter().enumerate() {
            assert!(
                req.at - first < SHORT_BUDGET,
                "POST {i} started {:?} after the first",
                req.at - first
            );
        }
    }

    /// A retried send is bounded: the 503's retry is never answered, and the respond
    /// gives up at that POST's wait, inside the deadline, instead of waiting forever.
    #[tokio::test]
    async fn respond_deadline_covers_a_stalled_retry() {
        let srv =
            ScriptedServer::spawn_replies(vec![retry_after(503, "0").into()], Reply::Silent).await;
        let env = Envelope::new_response("r", "ns", serde_json::Value::Null);
        let started = Instant::now();
        let r = tokio::time::timeout(
            Duration::from_secs(20),
            short_respond(open_client(&srv.url())).respond("ns", &env, &never_shutdown()),
        )
        .await
        .expect("respond returns at its deadline");
        assert!(matches!(r, Err(BusError::Transport(_))), "{r:?}");
        assert!(started.elapsed() < SHORT_BUDGET + Duration::from_millis(900));
        assert_eq!(srv.seen().len(), 2);
    }

    /// The deadline covers getting a credential: after a 401, a re-mint that takes far
    /// longer than the budget is abandoned at the deadline.
    #[tokio::test]
    async fn respond_deadline_covers_a_slow_re_mint() {
        struct SlowAfterInvalidate(AtomicBool);
        #[async_trait::async_trait]
        impl TokenProvider for SlowAfterInvalidate {
            async fn token(&self) -> Result<String, BusError> {
                if self.0.load(Ordering::SeqCst) {
                    tokio::time::sleep(Duration::from_secs(60)).await;
                }
                Ok("tok".into())
            }
            fn invalidate(&self) {
                self.0.store(true, Ordering::SeqCst);
            }
            fn would_mint(&self) -> bool {
                self.0.load(Ordering::SeqCst)
            }
        }
        let srv = ScriptedServer::spawn(vec![], Answer(401, vec![], String::new())).await;
        let client = short_respond(
            BusClient::new(
                srv.url(),
                String::new(),
                Some(Arc::new(SlowAfterInvalidate(AtomicBool::new(false)))),
                None,
                None,
                silent_log(),
            )
            .unwrap(),
        );
        let env = Envelope::new_response("r", "ns", serde_json::Value::Null);
        let started = Instant::now();
        let r = tokio::time::timeout(
            Duration::from_secs(20),
            client.respond("ns", &env, &never_shutdown()),
        )
        .await
        .expect("respond returns at its deadline");
        assert!(matches!(r, Err(BusError::Transport(_))), "{r:?}");
        assert!(started.elapsed() < SHORT_BUDGET + Duration::from_millis(900));
        assert_eq!(srv.seen().len(), 1, "the re-sent POST never went out");
    }

    #[test]
    fn redirect_allowed_table() {
        let u = |s: &str| reqwest::Url::parse(s).unwrap();
        let base = u("https://shed.example:8443/");
        let allowed = |next: &str, hops: usize| redirect_allowed(Some(&base), &u(next), hops);
        assert!(allowed("https://shed.example:8443/api/x", 1));
        assert!(allowed("https://SHED.example:8443/api/x", MAX_REDIRECTS));
        // Another origin, a downgrade, too many hops, no configured URL: stop.
        assert!(!allowed("https://shed.example:8444/api/x", 1));
        assert!(!allowed("https://other.example:8443/api/x", 1));
        assert!(!allowed("https://evil.shed.example:8443/api/x", 1));
        assert!(!allowed("http://shed.example:8443/api/x", 1));
        assert!(!allowed(
            "https://shed.example:8443/api/x",
            MAX_REDIRECTS + 1
        ));
        assert!(!redirect_allowed(None, &u("https://shed.example:8443/"), 1));
        // The default port is the same origin as the explicit one.
        let plain = u("https://shed.example/");
        assert!(redirect_allowed(
            Some(&plain),
            &u("https://shed.example:443/x"),
            1
        ));
        // A plaintext server follows nothing, even to itself.
        let open = u("http://127.0.0.1:8080/");
        assert!(!redirect_allowed(
            Some(&open),
            &u("http://127.0.0.1:8080/x"),
            1
        ));
    }

    /// A blocking HTTPS server on its own threads for the redirect tests: the pinned
    /// leaf, an OPTIONAL client certificate (so a client that has one presents it), and
    /// per request a record of its path, headers and whether a client certificate came
    /// with it. `accepts` counts TCP connections, so zero proves nothing at all arrived.
    struct TlsRecorder {
        port: u16,
        accepts: Arc<AtomicUsize>,
        seen: TlsSeen,
    }

    /// Per request: its path, its headers, and whether a client certificate came with it.
    type TlsSeen = Arc<Mutex<Vec<(String, Vec<(String, String)>, bool)>>>;
    /// `answer(path, own_port)`: the raw HTTP response for a request.
    type TlsAnswer = Arc<dyn Fn(&str, u16) -> String + Send + Sync>;

    #[derive(Debug)]
    struct AnyClientCert(rustls::crypto::WebPkiSupportedAlgorithms);

    impl rustls::server::danger::ClientCertVerifier for AnyClientCert {
        fn offer_client_auth(&self) -> bool {
            true
        }
        fn client_auth_mandatory(&self) -> bool {
            false
        }
        fn root_hint_subjects(&self) -> &[rustls::DistinguishedName] {
            &[]
        }
        fn verify_client_cert(
            &self,
            _end: &rustls::pki_types::CertificateDer<'_>,
            _intermediates: &[rustls::pki_types::CertificateDer<'_>],
            _now: rustls::pki_types::UnixTime,
        ) -> Result<rustls::server::danger::ClientCertVerified, rustls::Error> {
            Ok(rustls::server::danger::ClientCertVerified::assertion())
        }
        fn verify_tls12_signature(
            &self,
            message: &[u8],
            cert: &rustls::pki_types::CertificateDer<'_>,
            dss: &rustls::DigitallySignedStruct,
        ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
            rustls::crypto::verify_tls12_signature(message, cert, dss, &self.0)
        }
        fn verify_tls13_signature(
            &self,
            message: &[u8],
            cert: &rustls::pki_types::CertificateDer<'_>,
            dss: &rustls::DigitallySignedStruct,
        ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
            rustls::crypto::verify_tls13_signature(message, cert, dss, &self.0)
        }
        fn supported_verify_schemes(&self) -> Vec<rustls::SignatureScheme> {
            self.0.supported_schemes()
        }
    }

    /// The test server's leaf (DER) and key; the pin a client uses for it.
    fn test_leaf() -> (Vec<u8>, Vec<u8>, String) {
        let key = rcgen::KeyPair::generate().unwrap();
        let params = rcgen::CertificateParams::new(vec!["127.0.0.1".to_string()]).unwrap();
        let cert = params.self_signed(&key).unwrap();
        let der = cert.der().to_vec();
        let pin = shed_core::tls::fingerprint(&der);
        (der, key.serialize_der(), pin)
    }

    impl TlsRecorder {
        /// `answer(path, own_port)` is the raw HTTP response for a request.
        fn spawn(leaf: &(Vec<u8>, Vec<u8>, String), answer: TlsAnswer) -> TlsRecorder {
            use std::io::{Read, Write};
            let provider = Arc::new(rustls::crypto::ring::default_provider());
            let algs = provider.signature_verification_algorithms;
            let cfg = Arc::new(
                rustls::ServerConfig::builder_with_provider(provider)
                    .with_safe_default_protocol_versions()
                    .unwrap()
                    .with_client_cert_verifier(Arc::new(AnyClientCert(algs)))
                    .with_single_cert(
                        vec![rustls::pki_types::CertificateDer::from(leaf.0.clone())],
                        rustls::pki_types::PrivateKeyDer::Pkcs8(
                            rustls::pki_types::PrivatePkcs8KeyDer::from(leaf.1.clone()),
                        ),
                    )
                    .unwrap(),
            );
            let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
            let port = listener.local_addr().unwrap().port();
            let accepts = Arc::new(AtomicUsize::new(0));
            let seen: TlsSeen = Arc::default();
            let (accepts2, seen2) = (accepts.clone(), seen.clone());
            std::thread::spawn(move || {
                for tcp in listener.incoming() {
                    let Ok(tcp) = tcp else { continue };
                    accepts2.fetch_add(1, Ordering::SeqCst);
                    let (cfg, seen, answer) = (cfg.clone(), seen2.clone(), answer.clone());
                    std::thread::spawn(move || {
                        let conn = rustls::ServerConnection::new(cfg).unwrap();
                        let mut tls = rustls::StreamOwned::new(conn, tcp);
                        let mut buf = Vec::new();
                        let mut tmp = [0u8; 4096];
                        let head_end = loop {
                            match tls.read(&mut tmp) {
                                Ok(0) | Err(_) => return,
                                Ok(n) => buf.extend_from_slice(&tmp[..n]),
                            }
                            if let Some(i) = buf.windows(4).position(|w| w == b"\r\n\r\n") {
                                break i;
                            }
                        };
                        let head = String::from_utf8_lossy(&buf[..head_end]).to_string();
                        let mut lines = head.split("\r\n");
                        let path = lines
                            .next()
                            .unwrap_or("")
                            .split(' ')
                            .nth(1)
                            .unwrap_or("")
                            .to_string();
                        let headers: Vec<(String, String)> = lines
                            .filter_map(|l| l.split_once(':'))
                            .map(|(k, v)| (k.trim().to_string(), v.trim().to_string()))
                            .collect();
                        let len: usize = headers
                            .iter()
                            .find(|(k, _)| k.eq_ignore_ascii_case("content-length"))
                            .and_then(|(_, v)| v.parse().ok())
                            .unwrap_or(0);
                        while buf.len() < head_end + 4 + len {
                            match tls.read(&mut tmp) {
                                Ok(0) | Err(_) => break,
                                Ok(n) => buf.extend_from_slice(&tmp[..n]),
                            }
                        }
                        let cert = tls.conn.peer_certificates().is_some_and(|c| !c.is_empty());
                        seen.lock().unwrap().push((path.clone(), headers, cert));
                        let _ = tls.write_all(answer(&path, port).as_bytes());
                        tls.conn.send_close_notify();
                        let _ = tls.flush();
                    });
                }
            });
            TlsRecorder {
                port,
                accepts,
                seen,
            }
        }
    }

    fn redirect_to(location: &str) -> String {
        format!(
            "HTTP/1.1 307 Temporary Redirect\r\nLocation: {location}\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
        )
    }

    const NO_CONTENT: &str = "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n";

    /// An https bus client pinned to `leaf`, presenting a client certificate, with an
    /// identity: everything a redirect could leak. Its certificate names no user: it is
    /// armed directly, a layer below adoption's user rule.
    fn pinned_identity_client(port: u16, leaf: &(Vec<u8>, Vec<u8>, String)) -> BusClient {
        let key = rcgen::KeyPair::generate().unwrap();
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params
            .distinguished_name
            .push(rcgen::DnType::CommonName, "SHA256:agent");
        let client_leaf = params.self_signed(&key).unwrap();
        let certified = shed_core::tls::certified_key_from_pem(
            &shed_core::csr::pem_encode(shed_core::csr::PEM_LABEL_CERTIFICATE, client_leaf.der()),
            &key.serialize_der(),
        )
        .unwrap();
        let resolver = Arc::new(ClientCertResolver::new());
        resolver.set(Some(Arc::new(certified)));
        BusClient::new(
            format!("https://127.0.0.1:{port}"),
            String::new(),
            None,
            Some(leaf.2.clone()),
            Some(resolver),
            silent_log(),
        )
        .unwrap()
        .with_machine(Some(identity("laptop")))
    }

    /// The identity headers and the client certificate never reach another origin: a
    /// redirect to another HTTPS host — one even presenting the SAME pinned certificate —
    /// is not followed, and the redirect itself is the answer; the other host sees no
    /// connection at all. Within the configured origin a redirect is followed (headers
    /// and certificate kept), at most [`MAX_REDIRECTS`] times.
    #[tokio::test]
    async fn redirects_never_leave_the_configured_origin() {
        let leaf = test_leaf();
        let other = TlsRecorder::spawn(&leaf, Arc::new(|_: &str, _: u16| NO_CONTENT.to_string()));
        let other_port = other.port;
        let env = Envelope::new_response("r", "ns", serde_json::Value::Null);

        // Another origin: stopped, nothing reaches it.
        let home = TlsRecorder::spawn(
            &leaf,
            Arc::new(move |_: &str, _: u16| {
                redirect_to(&format!(
                    "https://127.0.0.1:{other_port}/api/plugins/listeners/ns/respond"
                ))
            }),
        );
        let client = pinned_identity_client(home.port, &leaf);
        let r = client.respond("ns", &env, &never_shutdown()).await;
        assert_eq!(
            other.accepts.load(Ordering::SeqCst),
            0,
            "another origin must see no connection: {:?}",
            other.seen.lock().unwrap()
        );
        assert!(matches!(r, Err(BusError::BadStatus(307, _))), "{r:?}");
        let home_seen = home.seen.lock().unwrap().clone();
        assert_eq!(home_seen.len(), 1);
        let (_, headers, cert) = &home_seen[0];
        assert!(*cert, "the configured server got the client certificate");
        assert!(headers
            .iter()
            .any(|(k, v)| k.eq_ignore_ascii_case(HEADER_MACHINE_ID) && v == MID));

        // The same origin: followed, with the headers and the certificate.
        let same = TlsRecorder::spawn(
            &leaf,
            Arc::new(|path: &str, port: u16| {
                if path == "/moved" {
                    NO_CONTENT.to_string()
                } else {
                    redirect_to(&format!("https://127.0.0.1:{port}/moved"))
                }
            }),
        );
        pinned_identity_client(same.port, &leaf)
            .respond("ns", &env, &never_shutdown())
            .await
            .expect("a same-origin redirect is followed");
        let seen = same.seen.lock().unwrap().clone();
        assert_eq!(
            seen.iter().map(|s| s.0.as_str()).collect::<Vec<_>>(),
            ["/api/plugins/listeners/ns/respond", "/moved"]
        );
        for (_, headers, cert) in &seen {
            assert!(*cert);
            assert!(headers
                .iter()
                .any(|(k, _)| k.eq_ignore_ascii_case(HEADER_MACHINE_ID)));
        }

        // A loop within the origin stops after MAX_REDIRECTS hops.
        let looping = TlsRecorder::spawn(
            &leaf,
            Arc::new(|_: &str, port: u16| redirect_to(&format!("https://127.0.0.1:{port}/again"))),
        );
        let r = pinned_identity_client(looping.port, &leaf)
            .respond("ns", &env, &never_shutdown())
            .await;
        assert!(matches!(r, Err(BusError::BadStatus(307, _))), "{r:?}");
        assert_eq!(looping.seen.lock().unwrap().len(), 1 + MAX_REDIRECTS);
    }

    /// A minter for the real [`crate::minter::CredentialSource`]: answers from a script
    /// (then fails), counting every mint attempt.
    struct ScriptedMinter {
        results: Mutex<std::collections::VecDeque<Result<String, String>>>,
        calls: AtomicUsize,
    }

    #[async_trait::async_trait]
    impl crate::minter::Minter for ScriptedMinter {
        async fn mint(
            &self,
            _target: &ServerTarget,
            _scope: &str,
        ) -> Result<crate::minter::Minted, crate::minter::MinterError> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            match self.results.lock().unwrap().pop_front() {
                Some(Ok(t)) => Ok(crate::minter::Minted::token(t, None)),
                Some(Err(e)) => Err(crate::minter::MinterError::new(e)),
                None => Err(crate::minter::MinterError::new("no more".into())),
            }
        }
    }

    /// At most ONE mint attempt per respond, against the real credential source: after a
    /// 401 the replacement mint fails (the source stays refused — its `rejected` flag is
    /// cleared only by a successful mint — and the old token is presented), the server
    /// answers 503 with `Retry-After`, and the retry presents the surviving token WITHOUT
    /// calling the minter again.
    #[tokio::test]
    async fn respond_makes_at_most_one_mint_attempt_with_the_real_source() {
        let minter = Arc::new(ScriptedMinter {
            results: Mutex::new(vec![Ok("tok-1".to_string()), Err("ssh down".to_string())].into()),
            calls: AtomicUsize::new(0),
        });
        let source = crate::minter::new_credential_source_with_store(
            minter.clone(),
            ServerTarget {
                name: "s".into(),
                url: String::new(),
                token: String::new(),
                tls_fingerprint: String::new(),
                ssh_host: String::new(),
                ssh_port: 0,
                auth_mode: String::new(),
                user: String::new(),
                identity_file: String::new(),
            },
            "credentials",
            None,
        );
        assert_eq!(source.token().await.unwrap(), "tok-1");
        assert_eq!(minter.calls.load(Ordering::SeqCst), 1);
        let srv = ScriptedServer::spawn(
            vec![
                Answer(401, vec![], api_error("UNAUTHORIZED")),
                retry_after(503, "0"),
            ],
            Answer(204, vec![], String::new()),
        )
        .await;
        let client = BusClient::new(
            srv.url(),
            String::new(),
            Some(Arc::new(source.clone())),
            None,
            None,
            silent_log(),
        )
        .unwrap();
        let env = Envelope::new_response("r", "ns", serde_json::Value::Null);
        client
            .respond("ns", &env, &never_shutdown())
            .await
            .expect("delivered on the retry");
        assert_eq!(
            minter.calls.load(Ordering::SeqCst),
            2,
            "one re-mint attempt for the whole respond, however many retries follow"
        );
        let auths: Vec<Option<String>> = srv
            .seen()
            .iter()
            .map(|r| r.header("authorization"))
            .collect();
        assert_eq!(
            auths,
            vec![Some("Bearer tok-1".to_string()); 3],
            "the surviving token"
        );
        // The source is still refused: the next respond may try one mint of its own.
        assert!(source.would_mint());
    }

    /// An answer the shed server itself refused (its own error body, read whole) is
    /// audited as undelivered, the operation's own result kept in the reason. Any other
    /// status — a proxy's 502 page, an empty 413, a server-shaped body cut short — keeps
    /// the operation's result and notes the status as "delivery unknown". A delivered
    /// answer is audited as before.
    #[tokio::test]
    async fn refused_answer_is_audited_as_undelivered() {
        let padded = format!(
            "{}{}x",
            api_error("FORBIDDEN"),
            " ".repeat(MAX_ERROR_BODY_BYTES)
        );
        let unknown = |st: u16| {
            (
                "ok".to_string(),
                format!("the response was answered with status {st}; delivery unknown"),
            )
        };
        for (answer, want) in [
            (
                Answer(403, vec![], not_bound_body("desk")),
                (
                    "undelivered".to_string(),
                    "result ok, but the server refused the response (status 403 SOURCE_NOT_BOUND)"
                        .to_string(),
                ),
            ),
            (
                // The server's own code for a respond body over its limit
                // (`internal/api/plugin_handlers.go`, `config.ErrRequestTooLarge`).
                Answer(413, vec![], api_error("REQUEST_TOO_LARGE")),
                (
                    "undelivered".to_string(),
                    "result ok, but the server refused the response (status 413 REQUEST_TOO_LARGE)"
                        .to_string(),
                ),
            ),
            (Answer(413, vec![], String::new()), unknown(413)),
            (
                Answer(502, vec![], "<html><body>Bad Gateway</body></html>".into()),
                unknown(502),
            ),
            (
                Answer(502, vec![], r#"{"error":"bad gateway"}"#.into()),
                unknown(502),
            ),
            (Answer(403, vec![], padded), unknown(403)),
            (
                Answer(204, vec![], String::new()),
                ("ok".into(), String::new()),
            ),
        ] {
            let status = answer.0;
            let srv = ScriptedServer::spawn(vec![answer], Answer(204, vec![], String::new())).await;
            let audit = Arc::new(CollectingAudit::default());
            let handlers = test_handlers(approve_gate(), audit.clone(), empty_backend());
            handle_bus_message(
                &open_client(&srv.url()),
                "ssh-agent",
                &list_request(),
                &never_shutdown(),
                &handlers,
            )
            .await;
            let entries = audit.entries.lock().unwrap().clone();
            assert_eq!(entries.len(), 1, "{status}");
            assert_eq!(
                (entries[0].result.clone(), entries[0].reason.clone()),
                want,
                "{status}"
            );
        }
        // The code the server gave is named, cleaned; an earlier reason is kept.
        let mut e = AuditEntry {
            result: "denied".into(),
            reason: "the person said no".into(),
            ..Default::default()
        };
        mark_refused(&mut e, 403, Some("SOURCE_NOT_BOUND\u{1b}"));
        assert_eq!(e.result, "undelivered");
        assert_eq!(
            e.reason,
            "the person said no; result denied, but the server refused the response (status 403 SOURCE_NOT_BOUND)"
        );
        let mut e = AuditEntry {
            result: "denied".into(),
            reason: "the person said no".into(),
            ..Default::default()
        };
        mark_refused(&mut e, 502, None);
        assert_eq!(e.result, "denied");
        assert_eq!(
            e.reason,
            "the person said no; the response was answered with status 502; delivery unknown"
        );
    }

    // ---- the respond deadline against the server's own (plan 035, whole-branch item 3) --

    /// The numbers, derived from the server's: each POST waits past the server's
    /// five-second delivery deadline with margin, and the budget holds a whole attempt,
    /// the server's `Retry-After`, and a second whole attempt.
    #[test]
    fn respond_timing_outlasts_the_servers_delivery_deadline() {
        let server_deadline = Duration::from_secs(SERVER_RESPONSE_DEADLINE_SECS);
        let retry_after = Duration::from_secs(SERVER_RESPOND_RETRY_AFTER_SECS);
        assert_eq!(
            server_deadline,
            Duration::from_secs(5),
            "bridge.go responseTimeout"
        );
        assert_eq!(
            retry_after,
            Duration::from_secs(1),
            "plugin_handlers.go Retry-After"
        );
        assert!(RESPOND_ATTEMPT_TIMEOUT >= server_deadline + Duration::from_secs(2));
        assert!(RESPOND_RETRY_BUDGET >= 2 * RESPOND_ATTEMPT_TIMEOUT + retry_after);
        assert_eq!(RESPOND_ATTEMPT_TIMEOUT, Duration::from_secs(8));
        assert_eq!(RESPOND_RETRY_BUDGET, Duration::from_secs(17));
        let c = open_client("http://x");
        assert_eq!(c.respond_attempt_timeout, RESPOND_ATTEMPT_TIMEOUT);
        assert_eq!(c.respond_budget, RESPOND_RETRY_BUDGET);
    }

    /// A server that holds the first answer behind its own delivery deadline, as the real
    /// one does (`internal/plugin/bridge.go`): just under it, the answer is delivered on
    /// the first POST; just over it, the server's `503 RESPONSE_NOT_DELIVERED` with
    /// `Retry-After: 1` arrives (this client is still waiting) and the answer is sent
    /// again and delivered. On the real numbers, not a shrunk budget.
    #[tokio::test]
    async fn respond_outlasts_the_servers_delivery_deadline_and_retries_its_503() {
        let deadline = Duration::from_secs(SERVER_RESPONSE_DEADLINE_SECS);
        let not_delivered = || {
            Answer(
                503,
                vec![("Retry-After", SERVER_RESPOND_RETRY_AFTER_SECS.to_string())],
                api_error("RESPONSE_NOT_DELIVERED"),
            )
        };
        let ok = || Answer(204, vec![], String::new());

        // Just under: delivered by the first POST, after the wait.
        let srv = ScriptedServer::spawn_replies(
            vec![Reply::Delayed {
                after: deadline - Duration::from_millis(100),
                answer: ok(),
            }],
            ok().into(),
        )
        .await;
        let audit = Arc::new(CollectingAudit::default());
        let handlers = test_handlers(approve_gate(), audit.clone(), empty_backend());
        handle_bus_message(
            &open_client(&srv.url()),
            "ssh-agent",
            &list_request(),
            &never_shutdown(),
            &handlers,
        )
        .await;
        assert_eq!(srv.seen().len(), 1, "delivered by the first POST");
        let entries = audit.entries.lock().unwrap().clone();
        assert_eq!(
            (entries[0].result.as_str(), entries[0].reason.as_str()),
            ("ok", "")
        );

        // Just over: the server's own 503 comes after its deadline; the retry delivers.
        let srv = ScriptedServer::spawn_replies(
            vec![Reply::Delayed {
                after: deadline + Duration::from_millis(100),
                answer: not_delivered(),
            }],
            ok().into(),
        )
        .await;
        let audit = Arc::new(CollectingAudit::default());
        let handlers = test_handlers(approve_gate(), audit.clone(), empty_backend());
        let started = Instant::now();
        handle_bus_message(
            &open_client(&srv.url()),
            "ssh-agent",
            &list_request(),
            &never_shutdown(),
            &handlers,
        )
        .await;
        let seen = srv.seen();
        assert_eq!(
            seen.len(),
            2,
            "the 503 after the server's deadline, then the retry"
        );
        assert!(
            seen[1].at - seen[0].at
                >= deadline + Duration::from_secs(SERVER_RESPOND_RETRY_AFTER_SECS),
            "the retry waited the server's Retry-After: {:?}",
            seen[1].at - seen[0].at
        );
        assert!(started.elapsed() < RESPOND_RETRY_BUDGET);
        let entries = audit.entries.lock().unwrap().clone();
        assert_eq!(
            (entries[0].result.as_str(), entries[0].reason.as_str()),
            ("ok", ""),
            "delivered on the retry: audited as delivered"
        );
    }

    /// A POST that gets no answer at all within its wait is "delivery unknown" in the
    /// audit (the answer may or may not have reached the shed), never a plain `ok`; a
    /// server's own refusal stays `undelivered`.
    #[tokio::test]
    async fn an_unanswered_respond_is_audited_delivery_unknown() {
        let srv = ScriptedServer::spawn_replies(vec![], Reply::Silent).await;
        let audit = Arc::new(CollectingAudit::default());
        let handlers = test_handlers(approve_gate(), audit.clone(), empty_backend());
        let client = open_client(&srv.url())
            .with_test_respond_timing(Duration::from_millis(300), Duration::from_secs(2));
        handle_bus_message(
            &client,
            "ssh-agent",
            &list_request(),
            &never_shutdown(),
            &handlers,
        )
        .await;
        let entries = audit.entries.lock().unwrap().clone();
        assert_eq!(entries.len(), 1);
        assert_eq!(
            entries[0].result, "ok",
            "what the operation came to is kept"
        );
        assert_eq!(
            entries[0].reason,
            "the response got no answer within 300ms; delivery unknown"
        );
        assert_eq!(srv.seen().len(), 1, "an unanswered POST is not re-sent");
    }

    // ---- subscribe honours 429 SOURCE_RATE_LIMITED's Retry-After (item 5) ----------------

    #[test]
    fn subscribe_retry_after_table() {
        use reqwest::header::HeaderValue;
        let v = |s: &str| HeaderValue::from_str(s).unwrap();
        assert_eq!(
            subscribe_retry_after(Some(&v("15"))),
            Some(Duration::from_secs(15))
        );
        assert_eq!(
            subscribe_retry_after(Some(&v(" 2 "))),
            Some(Duration::from_secs(2))
        );
        assert_eq!(subscribe_retry_after(Some(&v("0"))), Some(Duration::ZERO));
        // Capped at a minute, a number too large to hold included.
        assert_eq!(
            subscribe_retry_after(Some(&v("3600"))),
            Some(SUBSCRIBE_RETRY_AFTER_CAP)
        );
        assert_eq!(
            subscribe_retry_after(Some(&v("99999999999999999999999"))),
            Some(SUBSCRIBE_RETRY_AFTER_CAP)
        );
        for raw in [
            "",
            "-1",
            "1.5",
            "soon",
            "Wed, 21 Oct 2015 07:28:00 GMT",
            "1, 2",
        ] {
            assert_eq!(subscribe_retry_after(Some(&v(raw))), None, "{raw:?}");
        }
        assert_eq!(subscribe_retry_after(None), None);
        // Only the code makes a 429 a rate limit.
        assert_eq!(
            classify_subscribe_refusal(429, &api_error(CODE_SOURCE_RATE_LIMITED), true),
            SubscribeRefusal::RateLimited
        );
        for (body, complete) in [
            (api_error("TOO_MANY_REQUESTS"), true),
            (String::new(), true),
            (api_error(CODE_SOURCE_RATE_LIMITED), false),
        ] {
            assert_eq!(
                classify_subscribe_refusal(429, &body, complete),
                SubscribeRefusal::Other,
                "{body:?} complete={complete}"
            );
        }
        assert_eq!(
            classify_subscribe_refusal(403, &api_error(CODE_SOURCE_RATE_LIMITED), true),
            SubscribeRefusal::Other
        );
    }

    /// A `429 SOURCE_RATE_LIMITED` is retried no sooner than its `Retry-After`, though the
    /// backoff is milliseconds; a `Retry-After` shorter than the backoff never shortens it;
    /// a 429 without the code keeps the plain backoff.
    #[tokio::test]
    async fn subscribe_429_rate_limited_waits_its_retry_after() {
        let limited = |secs: &str| {
            Answer(
                429,
                vec![("Retry-After", secs.to_string())],
                api_error(CODE_SOURCE_RATE_LIMITED),
            )
        };
        // Retry-After 2 s against a 5 ms backoff.
        let srv =
            ScriptedServer::spawn(vec![limited("2")], Answer(200, vec![], sse_one("x"))).await;
        let client = fast(open_client(&srv.url())).with_machine(Some(identity("laptop")));
        let (tx, rx) = shutdown_pair();
        let sub = client.subscribe("ssh-agent", rx);
        let seen = srv.wait_for(2, Duration::from_secs(8)).await;
        let gap = seen[1].at - seen[0].at;
        assert!(
            gap >= Duration::from_secs(2),
            "waited the server's Retry-After: {gap:?}"
        );
        assert!(gap < Duration::from_secs(4), "{gap:?}");
        drop(sub);
        let _ = tx.send(true);

        // Retry-After 0 against a 400 ms backoff: the backoff stands.
        let srv =
            ScriptedServer::spawn(vec![limited("0")], Answer(200, vec![], sse_one("x"))).await;
        let client = open_client(&srv.url())
            .with_test_backoff(Duration::from_millis(400), Duration::from_millis(400))
            .with_machine(Some(identity("laptop")));
        let (tx, rx) = shutdown_pair();
        let _sub = client.subscribe("ssh-agent", rx);
        let seen = srv.wait_for(2, Duration::from_secs(5)).await;
        assert!(seen[1].at - seen[0].at >= Duration::from_millis(400));
        let _ = tx.send(true);

        // A 429 with another code: the 5 ms backoff, not two seconds.
        let srv = ScriptedServer::spawn(
            vec![Answer(
                429,
                vec![("Retry-After", "2".into())],
                api_error("TOO_MANY"),
            )],
            Answer(200, vec![], sse_one("x")),
        )
        .await;
        let client = fast(open_client(&srv.url())).with_machine(Some(identity("laptop")));
        let (tx, rx) = shutdown_pair();
        let _sub = client.subscribe("ssh-agent", rx);
        let seen = srv.wait_for(2, Duration::from_secs(5)).await;
        assert!(seen[1].at - seen[0].at < Duration::from_secs(1));
        let _ = tx.send(true);
    }

    // ---- a 429's Retry-After is not cut short by retry_now (review item ii) -------------

    /// A `429 SOURCE_RATE_LIMITED` with `Retry-After: 2`, and a `retry_now` (a source-use
    /// action waking the supervisor) 100 ms into that wait: the next subscribe still comes
    /// no sooner than the server asked. An ordinary backoff stays interruptible
    /// (`retry_now_cuts_the_backoff_short`).
    #[tokio::test]
    async fn retry_now_does_not_cut_a_rate_limit_wait_short() {
        let srv = ScriptedServer::spawn(
            vec![Answer(
                429,
                vec![("Retry-After", "2".into())],
                api_error(CODE_SOURCE_RATE_LIMITED),
            )],
            Answer(200, vec![], sse_one("x")),
        )
        .await;
        let client = fast(open_client(&srv.url())).with_machine(Some(identity("laptop")));
        let (tx, rx) = shutdown_pair();
        let _sub = client.subscribe("ssh-agent", rx);
        srv.wait_for(1, Duration::from_secs(5)).await;
        tokio::time::sleep(Duration::from_millis(100)).await;
        client.retry_now();
        let seen = srv.wait_for(2, Duration::from_secs(8)).await;
        let gap = seen[1].at - seen[0].at;
        assert!(
            gap >= Duration::from_secs(2),
            "the server's Retry-After is waited out whole: {gap:?}"
        );
        let _ = tx.send(true);
    }

    // ---- the answer POST: no retry without room for a whole attempt (review item iii) ---

    /// The retry rule: a retry starts only when its wait and then one whole attempt fit
    /// what is left of the budget.
    #[test]
    fn respond_retry_needs_room_for_a_whole_attempt() {
        use reqwest::header::HeaderValue;
        let one = HeaderValue::from_static("1");
        let s = Duration::from_secs;
        let attempt = RESPOND_ATTEMPT_TIMEOUT;
        assert_eq!(
            respond_retry_wait(503, Some(&one), 0, s(12), attempt),
            Some(s(1)),
            "1 + 8 fits 12"
        );
        assert_eq!(
            respond_retry_wait(503, Some(&one), 1, s(9), attempt),
            Some(s(1)),
            "1 + 8 fits 9 exactly"
        );
        assert_eq!(
            respond_retry_wait(503, Some(&one), 1, s(6), attempt),
            None,
            "1 + 8 does not fit 6: no attempt the budget would cut short"
        );
        assert_eq!(respond_retry_wait(429, Some(&one), 1, s(6), attempt), None);
    }

    /// A server that holds every answer past its delivery deadline and then answers
    /// `503 RESPONSE_NOT_DELIVERED` (scaled: it holds 600 ms, a POST waits 800 ms, the
    /// budget is two attempts and the minimum wait). The second 503 leaves less than a
    /// whole attempt, so no third POST starts, and the audit says what the server said —
    /// `undelivered` — never "delivery unknown" from an attempt the budget cut short.
    #[tokio::test]
    async fn the_budget_ending_after_503s_is_audited_undelivered() {
        let held = || Reply::Delayed {
            after: Duration::from_millis(600),
            answer: Answer(
                503,
                vec![("Retry-After", "0".into())],
                api_error("RESPONSE_NOT_DELIVERED"),
            ),
        };
        let srv = ScriptedServer::spawn_replies(vec![held(), held(), held()], held()).await;
        let audit = Arc::new(CollectingAudit::default());
        let handlers = test_handlers(approve_gate(), audit.clone(), empty_backend());
        let client = open_client(&srv.url()).with_test_respond_timing(
            Duration::from_millis(800),
            Duration::from_millis(2 * 800) + RESPOND_RETRY_MIN_WAIT,
        );
        handle_bus_message(
            &client,
            "ssh-agent",
            &list_request(),
            &never_shutdown(),
            &handlers,
        )
        .await;
        assert_eq!(srv.seen().len(), 2, "no third POST without room for it");
        let entries = audit.entries.lock().unwrap().clone();
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].result, AUDIT_RESULT_UNDELIVERED, "{entries:?}");
        assert!(
            entries[0].reason.contains("RESPONSE_NOT_DELIVERED"),
            "{entries:?}"
        );
        assert!(
            !entries[0].reason.contains("delivery unknown"),
            "{entries:?}"
        );
    }

    // ---- the daemon stands by while the desktop app's broker runs (plan 035 §3.4) ------

    /// The daemon's presence channel, set by hand (what the lock watcher publishes: each
    /// start of the app moves the yield epoch on).
    struct FakePresence {
        tx: watch::Sender<Presence>,
    }

    impl FakePresence {
        fn new(on: bool) -> Arc<FakePresence> {
            Arc::new(FakePresence {
                tx: watch::channel(Presence::default().next(on)).0,
            })
        }
        fn set(&self, on: bool) {
            self.tx.send_modify(|v| *v = v.next(on));
        }
        fn epoch(&self) -> u64 {
            self.tx.borrow().epoch
        }
        fn rx(&self) -> watch::Receiver<Presence> {
            self.tx.subscribe()
        }
    }

    /// One subscribe a [`SimBus`] saw.
    #[derive(Clone, Debug)]
    struct SimEvent {
        at: tokio::time::Instant,
        who: String,
        ns: String,
        /// It reached the server (not a refused connect).
        reached: bool,
        /// It holds the namespace now (not refused as a duplicate).
        ok: bool,
    }

    /// A [`SimBus`] hook, given the accepted client's name.
    type OnAccept = Arc<dyn Fn(&str) + Send + Sync>;

    #[derive(Default)]
    struct SimState {
        down: bool,
        /// namespace -> (stream id, holder)
        holders: HashMap<String, (u64, String)>,
        next: u64,
        events: Vec<SimEvent>,
        /// Run when a subscribe is accepted, before it is answered (the hook a race
        /// cell uses to act in the instant after a subscribe got through).
        on_accept: Option<OnAccept>,
        /// A 0.9.x server: a subscribe for a held namespace is the TERMINAL
        /// `409 NAMESPACE_ALREADY_REGISTERED`, not the retryable duplicate.
        old_server: bool,
    }

    /// An in-memory pinning server for one bound machine, on tokio's clock: what a client
    /// made by [`SimBus::client`] sends its subscribes to instead of HTTP (the credential
    /// is still fetched as for a real request). One live stream per namespace; another
    /// is `409 SOURCE_DUPLICATE_STREAM`. While it is down every connect is refused at once
    /// and every stream has ended. On the paused clock a test runs minutes of retries
    /// with the real backoff and presence constants in milliseconds.
    pub(super) struct SimBus {
        state: Mutex<SimState>,
        /// Bumped when streams may have ended.
        changed: watch::Sender<u64>,
    }

    fn sim_name(c: &BusClient) -> String {
        c.machine
            .as_ref()
            .map(|m| m.name.clone())
            .unwrap_or_default()
    }

    impl SimBus {
        fn new() -> Arc<SimBus> {
            Arc::new(SimBus {
                state: Mutex::default(),
                changed: watch::channel(0u64).0,
            })
        }

        /// A client of this bus presenting `name` (every client shares one machine id),
        /// with the real backoff: 1 s doubling to 30 s.
        fn client(self: &Arc<Self>, name: &str) -> BusClient {
            let mut c = open_client("http://127.0.0.1:9").with_machine(Some(identity(name)));
            c.sim = Some(self.clone());
            c
        }

        fn bump(&self) {
            self.changed.send_modify(|n| *n = n.wrapping_add(1));
        }

        /// The server goes away: every stream ends, every connect is refused.
        fn go_down(&self) {
            {
                let mut s = self.state.lock().unwrap();
                s.down = true;
                s.holders.clear();
            }
            self.bump();
        }

        fn come_up(&self) {
            self.state.lock().unwrap().down = false;
            self.bump();
        }

        /// Every stream ends, the server staying up.
        fn drop_streams(&self) {
            self.state.lock().unwrap().holders.clear();
            self.bump();
        }

        /// Answer a held namespace as a 0.9.x server does (terminal 409).
        fn old_server(&self) {
            self.state.lock().unwrap().old_server = true;
        }

        fn on_accept(&self, f: impl Fn(&str) + Send + Sync + 'static) {
            self.state.lock().unwrap().on_accept = Some(Arc::new(f));
        }

        fn holder(&self, ns: &str) -> Option<String> {
            self.state
                .lock()
                .unwrap()
                .holders
                .get(ns)
                .map(|(_, n)| n.clone())
        }

        fn events(&self) -> Vec<SimEvent> {
            self.state.lock().unwrap().events.clone()
        }

        /// `who`'s subscribes at or after `since` (refused connects included).
        fn attempts(&self, who: &str, since: tokio::time::Instant) -> Vec<SimEvent> {
            self.events()
                .into_iter()
                .filter(|e| e.who == who && e.at >= since)
                .collect()
        }

        pub(super) async fn subscribe(
            &self,
            client: &BusClient,
            ns: &str,
            shutdown: &watch::Receiver<bool>,
            status: &Arc<Mutex<SubStatus>>,
            connected: &AtomicBool,
        ) -> StreamEnd {
            let who = sim_name(client);
            // Watched from the moment it is accepted, so no drop is missed.
            let mut changed = self.changed.subscribe();
            let (id, hook) = {
                let mut s = self.state.lock().unwrap();
                if s.down {
                    s.events.push(SimEvent {
                        at: tokio::time::Instant::now(),
                        who: who.clone(),
                        ns: ns.to_string(),
                        reached: false,
                        ok: false,
                    });
                    return StreamEnd::Transport("connecting: connection refused".into());
                }
                let held = s.holders.contains_key(ns);
                let id = (!held).then(|| {
                    s.next += 1;
                    s.next
                });
                if let Some(id) = id {
                    s.holders.insert(ns.to_string(), (id, who.clone()));
                }
                s.events.push(SimEvent {
                    at: tokio::time::Instant::now(),
                    who: who.clone(),
                    ns: ns.to_string(),
                    reached: true,
                    ok: !held,
                });
                if held && s.old_server {
                    return StreamEnd::Conflict(format!(
                        "namespace already has an active subscriber: {{\"error\":{{\"code\":\
                         \"NAMESPACE_ALREADY_REGISTERED\",\"message\":\"namespace {ns} already \
                         has an active listener\"}}}}"
                    ));
                }
                (id, s.on_accept.clone().filter(|_| !held))
            };
            if let Some(hook) = hook {
                hook(&who);
            }
            let Some(id) = id else {
                return StreamEnd::Duplicate(format!(
                    "another process with this machine's id already holds namespace {ns} on \
                     this server (duplicate stream)"
                ));
            };
            connected.store(true, Ordering::SeqCst);
            set_state(status, ConnState::Connected, None);
            // Dropped (the presence watch won the race, or shutdown): the stream ends,
            // so its namespace is free again — as the server's handler unregisters it
            // when the connection closes.
            struct Release<'a>(&'a SimBus, &'a str, u64);
            impl Drop for Release<'_> {
                fn drop(&mut self) {
                    let mut s = self.0.state.lock().unwrap();
                    if s.holders.get(self.1).is_some_and(|(h, _)| *h == self.2) {
                        s.holders.remove(self.1);
                    }
                }
            }
            let _release = Release(self, ns, id);
            loop {
                let ours = |s: &SimState| s.holders.get(ns).is_some_and(|(h, _)| *h == id);
                if !ours(&self.state.lock().unwrap()) {
                    return StreamEnd::ClosedByServer("stream closed by server".into());
                }
                tokio::select! {
                    _ = wait_shutdown(shutdown.clone()) => return StreamEnd::Shutdown,
                    _ = changed.changed() => {}
                }
            }
        }
    }

    const PIN_NS: [&str; 2] = ["ssh-agent", "docker-credentials"];

    /// Let the (paused) clock run for `d`.
    async fn run_for(d: Duration) {
        tokio::time::sleep(d).await;
    }

    /// `client` subscribed to every [`PIN_NS`]; the subscriptions (kept alive) and the
    /// sender that stops them.
    fn subscribe_all(client: &BusClient) -> (watch::Sender<bool>, Vec<Subscription>) {
        let (tx, rx) = shutdown_pair();
        let subs = PIN_NS
            .iter()
            .map(|ns| client.subscribe(ns, rx.clone()))
            .collect();
        (tx, subs)
    }

    fn assert_yielding(subs: &[Subscription], ctx: &str) {
        for s in subs {
            let st = s.status();
            assert_eq!(st.state, ConnState::Yielding, "{ctx}: {st:?}");
            assert_eq!(st.last_error, YIELDING_REASON, "{ctx}");
        }
    }

    /// With the desktop app's broker already running when the daemon starts, the daemon
    /// never subscribes — not once, across server outages of a few seconds and of many
    /// minutes and drops with the server up — and says it is yielding. The app holds
    /// every type throughout.
    #[tokio::test(start_paused = true)]
    async fn present_before_the_first_subscribe_means_no_subscribe_ever() {
        let bus = SimBus::new();
        let start = tokio::time::Instant::now();
        let p = FakePresence::new(true);
        let (_app_tx, _app_subs) = subscribe_all(&bus.client("app"));
        let daemon = bus.client("daemon").with_presence(Some(p.rx()));
        let (_d_tx, d_subs) = subscribe_all(&daemon);
        run_for(Duration::from_secs(10)).await;
        assert_yielding(&d_subs, "at start");

        for (down, up) in [
            (Duration::from_secs(3), Duration::from_secs(40)),
            (Duration::from_secs(10 * 60), Duration::from_secs(90)),
        ] {
            bus.go_down();
            run_for(down).await;
            bus.come_up();
            run_for(up).await;
            bus.drop_streams();
            run_for(Duration::from_secs(60)).await;
        }
        assert!(
            bus.attempts("daemon", start).is_empty(),
            "the daemon never asked: {:?}",
            bus.attempts("daemon", start)
        );
        for ns in PIN_NS {
            assert_eq!(bus.holder(ns).as_deref(), Some("app"), "{ns}");
        }
        assert_yielding(&d_subs, "at the end");
    }

    /// The daemon holds every type; then the app starts — it takes the lock, then
    /// subscribes, and is refused as a duplicate, because the daemon's watcher has not
    /// looked since. At the watcher's next look (at most one interval later) the daemon
    /// closes every stream it holds AT ONCE, and the app's next retry holds each type;
    /// the daemon then stands by and never asks again while the app runs.
    #[tokio::test(start_paused = true)]
    async fn a_held_stream_is_closed_at_the_next_look_and_the_app_takes_the_type() {
        let bus = SimBus::new();
        let p = FakePresence::new(false);
        let daemon = bus.client("daemon").with_presence(Some(p.rx()));
        let (_d_tx, d_subs) = subscribe_all(&daemon);
        run_for(Duration::from_secs(95)).await;
        for ns in PIN_NS {
            assert_eq!(bus.holder(ns).as_deref(), Some("daemon"));
        }

        // The app starts: lock first, then its subscribes (refused: the daemon holds,
        // its watcher not having looked yet).
        let t0 = tokio::time::Instant::now();
        let (_app_tx, _app_subs) = subscribe_all(&bus.client("app"));
        run_for(Duration::from_millis(1)).await;
        assert!(
            bus.attempts("app", t0).iter().all(|e| !e.ok),
            "refused while the daemon held"
        );
        // The watcher's next look, at most an interval later.
        run_for(PRESENCE_CHECK_INTERVAL - Duration::from_millis(2)).await;
        p.set(true);
        run_for(Duration::from_millis(1)).await;
        for ns in PIN_NS {
            assert_ne!(
                bus.holder(ns).as_deref(),
                Some("daemon"),
                "{ns}: closed at once on the look"
            );
        }
        // The app's retries: 1 s, then 2 s after that.
        run_for(Duration::from_secs(3)).await;
        for ns in PIN_NS {
            assert_eq!(bus.holder(ns).as_deref(), Some("app"), "{ns}");
        }
        assert_yielding(&d_subs, "after the app took over");
        let after = tokio::time::Instant::now();
        run_for(Duration::from_secs(10 * 60)).await;
        assert!(
            bus.attempts("daemon", after).is_empty(),
            "{:?}",
            bus.attempts("daemon", after)
        );
    }

    /// The race the lock leaves: the watcher looked (no app), then the app takes the lock
    /// and the daemon's subscribe gets through in the same instant, so the daemon holds
    /// the type and the app is refused as a duplicate. At the watcher's next look the
    /// daemon closes its stream at once, and the app's next retry wins.
    #[tokio::test(start_paused = true)]
    async fn the_look_then_subscribe_race_resolves_at_the_next_look() {
        let bus = SimBus::new();
        let p = FakePresence::new(false);
        // The instant the daemon's subscribes have got through, the app takes its lock
        // (which the watcher sees only at its next look).
        let locked_at = Arc::new(Mutex::new(None));
        {
            let locked_at = locked_at.clone();
            let accepted = std::sync::atomic::AtomicUsize::new(0);
            bus.on_accept(move |who| {
                if who == "daemon" && accepted.fetch_add(1, Ordering::SeqCst) + 1 == PIN_NS.len() {
                    *locked_at.lock().unwrap() = Some(tokio::time::Instant::now());
                }
            });
        }
        let daemon = bus.client("daemon").with_presence(Some(p.rx()));
        let (_d_tx, d_subs) = subscribe_all(&daemon);
        run_for(Duration::from_millis(1)).await;
        assert!(locked_at.lock().unwrap().is_some());
        let t0 = tokio::time::Instant::now();
        for ns in PIN_NS {
            assert_eq!(
                bus.holder(ns).as_deref(),
                Some("daemon"),
                "the daemon got in"
            );
        }
        let (_app_tx, _app_subs) = subscribe_all(&bus.client("app"));
        run_for(Duration::from_millis(1)).await;
        assert!(bus.attempts("app", t0).iter().all(|e| e.reached && !e.ok));
        run_for(PRESENCE_CHECK_INTERVAL - Duration::from_millis(3)).await;
        p.set(true); // the watcher's next look
        run_for(Duration::from_millis(1)).await;
        for ns in PIN_NS {
            assert_ne!(bus.holder(ns).as_deref(), Some("daemon"), "{ns}");
        }
        run_for(Duration::from_secs(3)).await;
        for ns in PIN_NS {
            assert_eq!(
                bus.holder(ns).as_deref(),
                Some("app"),
                "{ns}: the app's retry won"
            );
        }
        assert_yielding(&d_subs, "after the race");
    }

    /// The app quits (the lock is released): the daemon subscribes within one check
    /// interval, from the backoff floor — though it had backed off to the ceiling while
    /// the server was away before the app started — so a drop right after it connected
    /// is retried a second later, not thirty.
    #[tokio::test(start_paused = true)]
    async fn released_means_subscribe_at_once_with_a_fresh_backoff() {
        let bus = SimBus::new();
        let p = FakePresence::new(false);
        bus.go_down();
        let daemon = bus.client("daemon").with_presence(Some(p.rx()));
        let (_d_tx, d_subs) = subscribe_all(&daemon);
        // Down for two minutes: the backoff grows to its ceiling.
        run_for(Duration::from_secs(120)).await;
        p.set(true);
        bus.come_up();
        // The daemon's next attempt (within the ceiling) looks first and stands by.
        run_for(MAX_BACKOFF + Duration::from_secs(1)).await;
        assert_yielding(&d_subs, "while the app runs");
        let quiet_since = tokio::time::Instant::now();
        run_for(Duration::from_secs(60)).await;
        assert!(bus.attempts("daemon", quiet_since).is_empty());

        p.set(false);
        let quit = tokio::time::Instant::now();
        run_for(PRESENCE_CHECK_INTERVAL + Duration::from_millis(1)).await;
        for ns in PIN_NS {
            assert_eq!(bus.holder(ns).as_deref(), Some("daemon"), "{ns}");
        }
        for s in &d_subs {
            assert_eq!(s.status().state, ConnState::Connected);
        }
        let first = bus.attempts("daemon", quit);
        assert!(
            first
                .iter()
                .all(|e| e.at - quit <= PRESENCE_CHECK_INTERVAL && e.ok),
            "{first:?}"
        );

        // A drop right away: the retry waits the floor, not the ceiling.
        bus.drop_streams();
        let dropped = tokio::time::Instant::now();
        run_for(INITIAL_BACKOFF + Duration::from_millis(1)).await;
        let again = bus.attempts("daemon", dropped);
        let mut asked: Vec<&str> = again.iter().map(|e| e.ns.as_str()).collect();
        asked.sort_unstable();
        assert_eq!(asked, ["docker-credentials", "ssh-agent"], "{again:?}");
        assert!(again.iter().all(|e| e.at - dropped <= INITIAL_BACKOFF));
    }

    /// Without a presence check (the embedded broker, every other host and test) the
    /// client never asks one, and holds a type whatever another process's check would say.
    #[tokio::test(start_paused = true)]
    async fn a_client_without_the_option_never_checks() {
        assert!(
            open_client("http://x").presence.is_none(),
            "the default is none"
        );
        let bus = SimBus::new();
        let app = bus.client("app").with_presence(None);
        let (_tx, subs) = subscribe_all(&app);
        run_for(Duration::from_secs(60)).await;
        for ns in PIN_NS {
            assert_eq!(bus.holder(ns).as_deref(), Some("app"));
        }
        for s in &subs {
            assert_eq!(s.status().state, ConnState::Connected);
        }
    }

    /// The daemon's wiring: `spawn_server_group` hands `SharedDeps.embedded_presence` to
    /// the bus client, so with it set and saying present the group's subscriptions stand
    /// by without asking the server anything; with it unset the group connects, whatever
    /// that channel says.
    #[tokio::test]
    async fn the_group_takes_its_presence_check_from_the_shared_deps() {
        let srv = ScriptedServer::spawn(vec![], Answer(200, vec![], sse_one("x"))).await;
        let p = FakePresence::new(true);
        let deps = |presence: Option<watch::Receiver<Presence>>| crate::supervisor::SharedDeps {
            ssh_backend: empty_backend(),
            ssh_gate: approve_gate(),
            ssh_sign_policy: Arc::new(SignPolicy::default()),
            aws: None,
            docker: None,
            audit: Arc::new(CollectingAudit::default()),
            minter: None,
            log: silent_log(),
            machine: Some(identity("laptop")),
            embedded_presence: presence,
            presence_lock: None,
        };
        let target = crate::discovery::ServerTarget {
            name: String::new(),
            url: srv.url(),
            token: String::new(),
            tls_fingerprint: String::new(),
            ssh_host: String::new(),
            ssh_port: 0,
            auth_mode: String::new(),
            user: String::new(),
            identity_file: String::new(),
        };

        let (tx, rx) = shutdown_pair();
        let g = spawn_server_group(rx, &target, &deps(Some(p.rx())));
        tokio::time::sleep(Duration::from_millis(300)).await;
        for s in &g.statuses {
            assert_eq!(s.lock().unwrap().state, ConnState::Yielding);
        }
        assert!(
            srv.seen().iter().all(|r| !r.path.contains("/api/plugins/")),
            "nothing subscribed: {:?}",
            srv.seen()
                .iter()
                .map(|r| r.path.clone())
                .collect::<Vec<_>>()
        );
        let _ = tx.send(true);
        let _ = g.done.await;

        let (tx, rx) = shutdown_pair();
        let g = spawn_server_group(rx, &target, &deps(None));
        let subscribe = "/api/plugins/listeners/ssh-agent/messages";
        let mut subscribed = false;
        for _ in 0..100 {
            if srv.seen().iter().any(|r| r.path == subscribe) {
                subscribed = true;
                break;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        assert!(subscribed, "the unset group subscribes");
        assert!(
            p.rx().borrow().present,
            "though the channel it was not given says present"
        );
        let _ = tx.send(true);
        let _ = g.done.await;
    }

    /// The static path of a group that does not mint (plan 037 A8): an entry that names
    /// an `identity_file` never sends its stored `credentials_token` — not on the bus and
    /// not on the egress stream; the same entry without one sends it on both, as before.
    #[tokio::test]
    async fn a_group_for_an_entry_with_an_identity_file_sends_no_stored_token() {
        let subscribe = "/api/plugins/listeners/ssh-agent/messages";
        let egress = "/api/egress/stream";
        for (identity_file, want) in [
            ("/home/pat/.ssh/id_work", None),
            ("", Some("Bearer stored-tok".to_string())),
        ] {
            let srv = ScriptedServer::spawn(vec![], Answer(200, vec![], sse_one("x"))).await;
            let deps = crate::supervisor::SharedDeps {
                ssh_backend: empty_backend(),
                ssh_gate: approve_gate(),
                ssh_sign_policy: Arc::new(SignPolicy::default()),
                aws: None,
                docker: None,
                audit: Arc::new(CollectingAudit::default()),
                minter: None,
                log: silent_log(),
                machine: Some(identity("laptop")),
                embedded_presence: None,
                presence_lock: None,
            };
            let target = crate::discovery::ServerTarget {
                name: "s".into(),
                url: srv.url(),
                token: "stored-tok".into(),
                tls_fingerprint: String::new(),
                ssh_host: String::new(),
                ssh_port: 0,
                auth_mode: String::new(),
                user: String::new(),
                identity_file: identity_file.into(),
            };
            assert_eq!(static_bearer(&target).is_empty(), want.is_none());
            let (tx, rx) = shutdown_pair();
            let g = spawn_server_group(rx, &target, &deps);
            let seen = |path: &str| srv.seen().iter().any(|r| r.path.starts_with(path));
            for _ in 0..250 {
                if seen(subscribe) && seen(egress) {
                    break;
                }
                tokio::time::sleep(Duration::from_millis(20)).await;
            }
            assert!(seen(subscribe) && seen(egress), "{identity_file:?}");
            for r in srv.seen() {
                assert_eq!(
                    r.header("authorization"),
                    want,
                    "{identity_file:?}: {}",
                    r.path
                );
            }
            let _ = tx.send(true);
            let _ = g.done.await;
        }
    }

    // ---- the reconnect after a long healthy stream (review item i) ----------------------

    /// Failures grow the backoff to its ceiling; then a stream holds longer than
    /// [`HELD_RESET`] and the server closes it: the reconnect waits the reset floor, not
    /// the old ceiling.
    #[tokio::test(start_paused = true)]
    async fn a_reconnect_after_a_long_healthy_stream_waits_the_reset_backoff() {
        let bus = SimBus::new();
        bus.go_down();
        let c = bus.client("laptop");
        let (_tx, rx) = shutdown_pair();
        let _sub = c.subscribe("ssh-agent", rx);
        run_for(Duration::from_secs(120)).await;
        bus.come_up();
        run_for(MAX_BACKOFF + Duration::from_secs(1)).await;
        assert_eq!(bus.holder("ssh-agent").as_deref(), Some("laptop"));
        run_for(HELD_RESET + Duration::from_secs(5)).await;

        bus.drop_streams();
        let dropped = tokio::time::Instant::now();
        run_for(MAX_BACKOFF + Duration::from_secs(1)).await;
        let next = bus.attempts("laptop", dropped);
        assert!(!next.is_empty());
        let gap = next[0].at - dropped;
        assert!(
            gap <= INITIAL_BACKOFF,
            "the reconnect waited the reset backoff, not the old one: {gap:?}"
        );
    }

    // ---- the backoff sleeps watch the lock (review item 4) ------------------------------

    /// The review's sequence: the daemon is in a 30 s backoff sleep (the server was away)
    /// when the app starts at second 1 and runs until second 10. The daemon reads
    /// `yielding` from the moment the watcher says so, not after the sleep; and the
    /// moment the app is gone it subscribes, from the floor, not at second 30.
    #[tokio::test(start_paused = true)]
    async fn a_backoff_sleep_yields_at_once_and_takes_over_at_once() {
        let bus = SimBus::new();
        let p = FakePresence::new(false);
        bus.go_down();
        let daemon = bus.client("daemon").with_presence(Some(p.rx()));
        let (_tx, subs) = subscribe_all(&daemon);
        // Down long enough that the backoff is at its ceiling, then just past an attempt:
        // the daemon is now at the start of a 30 s sleep.
        run_for(Duration::from_secs(120)).await;
        let last = bus
            .attempts("daemon", tokio::time::Instant::now() - MAX_BACKOFF)
            .last()
            .map(|e| e.at)
            .expect("attempts while down");
        // Just past the next attempt: a whole 30 s sleep lies ahead.
        let next = last + MAX_BACKOFF;
        run_for(next - tokio::time::Instant::now() + Duration::from_millis(1)).await;
        assert_eq!(
            bus.attempts("daemon", next).len(),
            PIN_NS.len(),
            "an attempt per type at the ceiling"
        );
        let sleep_ends = next + MAX_BACKOFF;
        bus.come_up();
        run_for(Duration::from_secs(1)).await;
        p.set(true); // the app starts
        run_for(Duration::from_millis(1)).await;
        assert_yielding(&subs, "while the app runs, in the middle of the sleep");
        run_for(Duration::from_secs(9)).await;
        assert!(
            tokio::time::Instant::now() < sleep_ends,
            "still inside the old sleep"
        );
        let quit = tokio::time::Instant::now();
        p.set(false); // the app quits
        run_for(Duration::from_millis(1)).await;
        for ns in PIN_NS {
            assert_eq!(bus.holder(ns).as_deref(), Some("daemon"), "{ns}: at once");
        }
        let got = bus.attempts("daemon", quit);
        assert!(
            got.iter().all(|e| e.at - quit <= Duration::from_millis(1)),
            "{got:?}"
        );
        for s in &subs {
            assert_eq!(s.status().state, ConnState::Connected);
        }
    }

    // ---- an older server's terminal 409 during the handoff (review item 5) --------------

    /// On a 0.9.x server the daemon holds the type; the app has taken the lock and
    /// subscribes before the daemon's next look, and is answered the TERMINAL 409. The
    /// embedded broker (holding the lock) retries on its backoff inside the grace, and
    /// holds the type once the daemon's stream is gone.
    #[tokio::test(start_paused = true)]
    async fn the_embedded_broker_retries_an_older_servers_409_inside_the_grace() {
        let bus = SimBus::new();
        bus.old_server();
        let p = FakePresence::new(false);
        let daemon = bus.client("daemon").with_presence(Some(p.rx()));
        let (_d_tx, d_subs) = subscribe_all(&daemon);
        run_for(Duration::from_secs(5)).await;
        let (_lock_tx, held) = watch::channel(Some(tokio::time::Instant::now()));
        let app = bus.client("app").with_presence_lock(Some(held));
        let t0 = tokio::time::Instant::now();
        let (_a_tx, a_subs) = subscribe_all(&app);
        run_for(Duration::from_millis(1)).await;
        assert!(bus.attempts("app", t0).iter().all(|e| e.reached && !e.ok));
        for s in &a_subs {
            assert_eq!(s.status().state, ConnState::Reconnecting, "not terminal");
        }
        // The daemon's next look (two seconds on).
        run_for(PRESENCE_CHECK_INTERVAL).await;
        p.set(true);
        run_for(Duration::from_secs(4)).await;
        for ns in PIN_NS {
            assert_eq!(bus.holder(ns).as_deref(), Some("app"), "{ns}");
        }
        for s in &a_subs {
            assert_eq!(s.status().state, ConnState::Connected);
        }
        assert_yielding(&d_subs, "the daemon stood by");
    }

    /// Past the grace the older server's 409 is terminal again, as before: an app whose
    /// lock was taken long ago, against another stream that never lets go, stops after
    /// the grace and asks no more.
    #[tokio::test(start_paused = true)]
    async fn past_the_grace_an_older_servers_409_is_terminal() {
        let bus = SimBus::new();
        bus.old_server();
        let other = bus.client("other-machine");
        let (_o_tx, _o_subs) = subscribe_all(&other);
        run_for(Duration::from_millis(1)).await;
        let (_lock_tx, held) = watch::channel(Some(tokio::time::Instant::now()));
        let app = bus.client("app").with_presence_lock(Some(held));
        let t0 = tokio::time::Instant::now();
        let (_a_tx, a_subs) = subscribe_all(&app);
        run_for(OLD_SERVER_CONFLICT_GRACE + MAX_BACKOFF).await;
        for s in &a_subs {
            assert_eq!(
                s.status().state,
                ConnState::Rejected,
                "terminal past the grace"
            );
        }
        let tried = bus.attempts("app", t0);
        assert!(
            tried.iter().filter(|e| e.ns == "ssh-agent").count() > 1,
            "retried inside the grace: {tried:?}"
        );
        assert!(
            tried
                .iter()
                .all(|e| e.at - t0 < OLD_SERVER_CONFLICT_GRACE + Duration::from_secs(1)),
            "nothing after the grace: {tried:?}"
        );
        let after = tokio::time::Instant::now();
        run_for(Duration::from_secs(10 * 60)).await;
        assert!(bus.attempts("app", after).is_empty());
    }

    /// A client without the option (the daemon, every other host) is unchanged: an older
    /// server's 409 stops it at the first answer.
    #[tokio::test(start_paused = true)]
    async fn without_the_presence_lock_an_older_servers_409_is_terminal_at_once() {
        let bus = SimBus::new();
        bus.old_server();
        let (_o_tx, _o_subs) = subscribe_all(&bus.client("other"));
        run_for(Duration::from_millis(1)).await;
        let t0 = tokio::time::Instant::now();
        let (_tx, subs) = subscribe_all(&bus.client("plain"));
        run_for(Duration::from_secs(60)).await;
        for s in &subs {
            assert_eq!(s.status().state, ConnState::Rejected);
        }
        assert_eq!(bus.attempts("plain", t0).len(), PIN_NS.len(), "one each");
    }

    /// The embedded broker started without the lock (it could not take it within its
    /// wait) is refused by an older server: rejected, until it takes the lock, when the
    /// daemon lets go — then it retries at once and, inside the grace counted from the
    /// lock, holds the type.
    #[tokio::test(start_paused = true)]
    async fn an_older_servers_409_before_the_lock_is_retried_once_it_is_taken() {
        let bus = SimBus::new();
        bus.old_server();
        let p = FakePresence::new(false);
        let daemon = bus.client("daemon").with_presence(Some(p.rx()));
        let (_d_tx, _d_subs) = subscribe_all(&daemon);
        run_for(Duration::from_millis(1)).await;
        let (lock_tx, held) = watch::channel(None);
        let app = bus.client("app").with_presence_lock(Some(held));
        let (_a_tx, a_subs) = subscribe_all(&app);
        run_for(Duration::from_secs(60)).await;
        for s in &a_subs {
            assert_eq!(s.status().state, ConnState::Rejected, "no lock: as before");
        }
        // The lock is taken a minute in; the daemon sees it at its next look.
        lock_tx.send_replace(Some(tokio::time::Instant::now()));
        run_for(PRESENCE_CHECK_INTERVAL).await;
        p.set(true);
        run_for(Duration::from_secs(8)).await;
        for ns in PIN_NS {
            assert_eq!(bus.holder(ns).as_deref(), Some("app"), "{ns}");
        }
    }

    // ---- the daemon stops answering when it yields (review item 3) ----------------------

    /// A subscription fed by hand: what `serve_namespace` drains.
    fn fed_subscription() -> (mpsc::Sender<Received>, Subscription) {
        let (tx, rx) = mpsc::channel(ENVELOPE_CHANNEL_CAP);
        let status = Arc::new(Mutex::new(SubStatus {
            namespace: "ssh-agent".into(),
            state: ConnState::Connected,
            last_error: String::new(),
            since: String::new(),
            active_source: None,
        }));
        let handle = tokio::spawn(async {});
        (
            tx,
            Subscription {
                rx: EnvelopeRx { rx },
                status,
                handle,
            },
        )
    }

    /// `env` as a stream of yield epoch `epoch` hands it on.
    fn on_stream(env: Envelope, epoch: u64) -> Received {
        Received { env, epoch }
    }

    fn list_request_id(id: &str) -> Envelope {
        let mut env = list_request();
        env.id = id.into();
        env
    }

    fn respond_posts(srv: &ScriptedServer) -> usize {
        srv.seen()
            .iter()
            .filter(|r| r.method == "POST" && r.path.ends_with("/respond"))
            .count()
    }

    /// Requests received but not started when the app takes over are left to it: no
    /// handler runs (no audit) and nothing is posted. Once the lock is released, a new
    /// request is answered as usual.
    #[tokio::test]
    async fn queued_requests_are_left_to_the_app_and_answering_resumes_after_it() {
        let srv = ScriptedServer::spawn(vec![], Answer(204, vec![], String::new())).await;
        let p = FakePresence::new(true);
        let client = open_client(&srv.url()).with_presence(Some(p.rx()));
        let audit = Arc::new(CollectingAudit::default());
        let handlers = Arc::new(test_handlers(
            approve_gate(),
            audit.clone(),
            empty_backend(),
        ));
        let (tx, sub) = fed_subscription();
        let (stop_tx, stop_rx) = shutdown_pair();
        let serve = tokio::spawn(serve_namespace(
            client,
            sub,
            "ssh-agent",
            stop_rx,
            handlers,
            silent_log(),
        ));
        for i in 0..3 {
            tx.send(on_stream(
                list_request_id(&format!("queued-{i}")),
                p.epoch(),
            ))
            .await
            .unwrap();
        }
        // All three taken off the channel (each is dealt with before the next is taken).
        while tx.capacity() < ENVELOPE_CHANNEL_CAP {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        // Serialised behind the three: once this one is answered, they were dealt with.
        p.set(false);
        tx.send(on_stream(list_request_id("after"), p.epoch()))
            .await
            .unwrap();
        for _ in 0..100 {
            if respond_posts(&srv) >= 1 && !audit.entries.lock().unwrap().is_empty() {
                break;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        assert_eq!(respond_posts(&srv), 1, "only the request after the release");
        assert_eq!(
            audit.entries.lock().unwrap().len(),
            1,
            "no handler ran for the others"
        );
        let _ = stop_tx.send(true);
        let _ = serve.await;
    }

    /// A gate that holds its answer until told, saying when it has been asked.
    struct HeldGate {
        asked: tokio::sync::Notify,
        release: tokio::sync::Notify,
    }
    #[async_trait::async_trait]
    impl ApprovalGate for HeldGate {
        async fn approve(
            &self,
            _ns: &str,
            _op: &str,
            _server: &str,
            _shed: &str,
            _detail: &str,
        ) -> crate::approval::ApprovalOutcome {
            self.asked.notify_one();
            self.release.notified().await;
            crate::approval::ApprovalOutcome {
                approved: true,
                decided_by: "user".into(),
                scope: None,
                ttl: None,
                reason: String::new(),
            }
        }
        fn method(&self) -> &str {
            "shed-desktop"
        }
    }

    /// A handler already running when the app takes over (here waiting on the approval)
    /// finishes, posts nothing, and writes one audit entry saying the request was left
    /// for the desktop app (`undelivered`, the operation's own result in the reason).
    #[tokio::test]
    async fn a_handler_that_finishes_after_the_app_took_over_posts_nothing() {
        let srv = ScriptedServer::spawn(vec![], Answer(204, vec![], String::new())).await;
        let p = FakePresence::new(false);
        let client = open_client(&srv.url()).with_presence(Some(p.rx()));
        let pub_bytes = fixed_ed25519_pub();
        let backend: Arc<dyn SshBackend> = Arc::new(StubBackend {
            pubkey: pub_bytes.clone(),
            result: Ok(SshSignature {
                format: "ssh-ed25519".to_string(),
                blob: vec![0x11; 64],
            }),
        });
        let gate = Arc::new(HeldGate {
            asked: tokio::sync::Notify::new(),
            release: tokio::sync::Notify::new(),
        });
        let audit = Arc::new(CollectingAudit::default());
        let handlers = test_handlers(gate.clone(), audit.clone(), backend);
        let env = sign_env(&b64(&pub_bytes), &b64(&git_login()), 0);
        let handler = tokio::spawn(async move {
            handle_bus_message(&client, "ssh-agent", &env, &never_shutdown(), &handlers).await;
        });
        gate.asked.notified().await;
        p.set(true); // the app takes over while the person is approving
        gate.release.notify_one();
        handler.await.unwrap();
        assert_eq!(
            respond_posts(&srv),
            0,
            "the daemon's answer is never posted"
        );
        let entries = audit.entries.lock().unwrap().clone();
        assert_eq!(entries.len(), 1, "{entries:?}");
        assert_eq!(entries[0].result, AUDIT_RESULT_UNDELIVERED, "{entries:?}");
        assert!(
            entries[0].reason.contains(AUDIT_NOTE_LEFT_FOR_APP)
                && entries[0].reason.starts_with("result ok, but not sent"),
            "{entries:?}"
        );
    }

    // ---- every send but the first needs a whole attempt (review item 7) -----------------

    /// The review's sequence, scaled (attempt 1600 ms, budget two attempts and the minimum
    /// wait): the first POST is answered 401 after 1200 ms (400 ms inside its attempt);
    /// the re-mint takes 1200 ms; the re-send would start with 900 ms left, 700 ms short
    /// of an attempt — so it is not made, and the audit says what the server said (the
    /// 401), not "delivery unknown".
    #[tokio::test]
    async fn a_resend_after_a_late_401_and_a_slow_re_mint_is_not_made() {
        struct SlowReMint(AtomicBool);
        #[async_trait::async_trait]
        impl TokenProvider for SlowReMint {
            async fn token(&self) -> Result<String, BusError> {
                if self.0.load(Ordering::SeqCst) {
                    tokio::time::sleep(Duration::from_millis(1200)).await;
                }
                Ok("tok".into())
            }
            fn invalidate(&self) {
                self.0.store(true, Ordering::SeqCst);
            }
            fn would_mint(&self) -> bool {
                self.0.load(Ordering::SeqCst)
            }
        }
        let late_401 = Reply::Delayed {
            after: Duration::from_millis(1200),
            answer: Answer(401, vec![], api_error("UNAUTHORIZED")),
        };
        let srv = ScriptedServer::spawn_replies(
            vec![late_401],
            Answer(204, vec![], String::new()).into(),
        )
        .await;
        let client = BusClient::new(
            srv.url(),
            String::new(),
            Some(Arc::new(SlowReMint(AtomicBool::new(false)))),
            None,
            None,
            silent_log(),
        )
        .unwrap()
        .with_test_respond_timing(
            Duration::from_millis(1600),
            Duration::from_millis(2 * 1600) + RESPOND_RETRY_MIN_WAIT,
        );
        let audit = Arc::new(CollectingAudit::default());
        let handlers = test_handlers(approve_gate(), audit.clone(), empty_backend());
        handle_bus_message(
            &client,
            "ssh-agent",
            &list_request(),
            &never_shutdown(),
            &handlers,
        )
        .await;
        assert_eq!(
            srv.seen().len(),
            1,
            "no re-send without a whole attempt left"
        );
        let entries = audit.entries.lock().unwrap().clone();
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].result, AUDIT_RESULT_UNDELIVERED, "{entries:?}");
        assert!(
            entries[0].reason.contains("status 401 UNAUTHORIZED"),
            "{entries:?}"
        );
    }

    /// A 503 retry whose credential fetch, after the wait, eats into the room for its
    /// attempt is not sent either; the audit says the 503 the server answered.
    #[tokio::test]
    async fn a_retry_whose_credential_takes_the_room_is_not_sent() {
        struct SlowSecond(AtomicUsize);
        #[async_trait::async_trait]
        impl TokenProvider for SlowSecond {
            async fn token(&self) -> Result<String, BusError> {
                if self.0.fetch_add(1, Ordering::SeqCst) == 1 {
                    tokio::time::sleep(Duration::from_millis(1000)).await;
                }
                Ok("tok".into())
            }
            fn invalidate(&self) {}
        }
        let srv = ScriptedServer::spawn(
            vec![Answer(
                503,
                vec![("Retry-After", "0".into())],
                api_error("RESPONSE_NOT_DELIVERED"),
            )],
            Answer(204, vec![], String::new()),
        )
        .await;
        let client = BusClient::new(
            srv.url(),
            String::new(),
            Some(Arc::new(SlowSecond(AtomicUsize::new(0)))),
            None,
            None,
            silent_log(),
        )
        .unwrap()
        .with_test_respond_timing(
            Duration::from_millis(800),
            Duration::from_millis(2 * 800) + RESPOND_RETRY_MIN_WAIT,
        );
        let audit = Arc::new(CollectingAudit::default());
        let handlers = test_handlers(approve_gate(), audit.clone(), empty_backend());
        handle_bus_message(
            &client,
            "ssh-agent",
            &list_request(),
            &never_shutdown(),
            &handlers,
        )
        .await;
        assert_eq!(srv.seen().len(), 1, "the retry was not sent");
        let entries = audit.entries.lock().unwrap().clone();
        assert_eq!(entries[0].result, AUDIT_RESULT_UNDELIVERED, "{entries:?}");
        assert!(
            entries[0]
                .reason
                .contains("status 503 RESPONSE_NOT_DELIVERED"),
            "{entries:?}"
        );
    }
    // ---- a yield is permanent for the daemon's old work (third review, item 3) ---------

    /// Wait (real clock, 5 s at most) until `cond` holds.
    async fn eventually(what: &str, mut cond: impl FnMut() -> bool) {
        for _ in 0..250 {
            if cond() {
                return;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        panic!("never happened: {what}");
    }

    fn audit_entries(audit: &CollectingAudit) -> Vec<AuditEntry> {
        audit.entries.lock().unwrap().clone()
    }

    /// The review's sequence: the daemon's answer is answered `503` with `Retry-After`
    /// (the server's own refusal: it wrote nothing); during the wait the app starts and
    /// quits again, the presence channel reading absent before and after (the start was
    /// never seen as `present` by the respond). The old answer is never re-sent, and the
    /// audit says it was left for the desktop app (`undelivered`: the one POST made was
    /// refused by the server itself).
    #[tokio::test]
    async fn an_answer_held_back_by_a_503_is_not_resent_after_the_app_came_and_went() {
        let srv = ScriptedServer::spawn(
            vec![Answer(
                503,
                vec![("Retry-After", "1".into())],
                api_error("RESPONSE_NOT_DELIVERED"),
            )],
            Answer(204, vec![], String::new()),
        )
        .await;
        let p = FakePresence::new(false);
        let client = open_client(&srv.url())
            .with_presence(Some(p.rx()))
            .with_test_respond_timing(Duration::from_millis(800), Duration::from_secs(5));
        let audit = Arc::new(CollectingAudit::default());
        let handlers = test_handlers(approve_gate(), audit.clone(), empty_backend());
        let handler = tokio::spawn(async move {
            handle_bus_message(
                &client,
                "ssh-agent",
                &list_request(),
                &never_shutdown(),
                &handlers,
            )
            .await;
        });
        eventually("the first POST", || respond_posts(&srv) == 1).await;
        // Inside the 1 s wait: the app starts and quits again.
        p.set(true);
        tokio::time::sleep(Duration::from_millis(50)).await;
        p.set(false);
        handler.await.unwrap();
        assert_eq!(respond_posts(&srv), 1, "the old answer is never re-sent");
        let entries = audit_entries(&audit);
        assert_eq!(entries.len(), 1, "{entries:?}");
        assert_eq!(entries[0].result, AUDIT_RESULT_UNDELIVERED, "{entries:?}");
        assert!(
            entries[0].reason.contains("but not sent: ")
                && entries[0].reason.contains(AUDIT_NOTE_LEFT_FOR_APP)
                && !entries[0].reason.contains("delivery unknown"),
            "{entries:?}"
        );
    }

    /// The same, but the first POST's refusal is not the server's own (a proxy's `503`,
    /// which can follow an answer that WAS delivered): that POST may have reached the
    /// shed, so the entry keeps the result and says delivery unknown, and that only the
    /// retry was left for the desktop app.
    #[tokio::test]
    async fn a_retry_left_for_the_app_after_a_proxy_refusal_is_delivery_unknown() {
        let srv = ScriptedServer::spawn(
            vec![Answer(
                503,
                vec![("Retry-After", "1".into())],
                "<html>bad gateway</html>".into(),
            )],
            Answer(204, vec![], String::new()),
        )
        .await;
        let p = FakePresence::new(false);
        let client = open_client(&srv.url())
            .with_presence(Some(p.rx()))
            .with_test_respond_timing(Duration::from_millis(800), Duration::from_secs(5));
        let audit = Arc::new(CollectingAudit::default());
        let handlers = test_handlers(approve_gate(), audit.clone(), empty_backend());
        let handler = tokio::spawn(async move {
            handle_bus_message(
                &client,
                "ssh-agent",
                &list_request(),
                &never_shutdown(),
                &handlers,
            )
            .await;
        });
        eventually("the first POST", || respond_posts(&srv) == 1).await;
        p.set(true); // the app takes over during the wait (and stays)
        handler.await.unwrap();
        assert_eq!(respond_posts(&srv), 1, "the retry is not sent");
        let entries = audit_entries(&audit);
        assert_eq!(entries.len(), 1, "{entries:?}");
        assert_ne!(entries[0].result, AUDIT_RESULT_UNDELIVERED, "{entries:?}");
        let reason = &entries[0].reason;
        assert!(
            reason.contains("an earlier send of the response was answered with status 503")
                && reason.contains("delivery unknown")
                && reason.contains(&format!("retry was not sent: {AUDIT_NOTE_LEFT_FOR_APP}")),
            "{entries:?}"
        );
    }

    /// A request received before a yield, queued behind a handler, is not handled once
    /// the app has come and gone: it is dropped from the queue when the app starts (while
    /// the handler still runs), and the handler's own answer is not posted. The same
    /// request delivered again on the daemon's new stream (the server re-delivers what is
    /// pending) IS handled and answered.
    #[tokio::test]
    async fn a_request_queued_before_a_yield_is_left_and_its_redelivery_is_answered() {
        let srv = ScriptedServer::spawn(vec![], Answer(204, vec![], String::new())).await;
        let p = FakePresence::new(false);
        let client = open_client(&srv.url()).with_presence(Some(p.rx()));
        let pub_bytes = fixed_ed25519_pub();
        let backend: Arc<dyn SshBackend> = Arc::new(StubBackend {
            pubkey: pub_bytes.clone(),
            result: Ok(SshSignature {
                format: "ssh-ed25519".to_string(),
                blob: vec![0x11; 64],
            }),
        });
        let gate = Arc::new(HeldGate {
            asked: tokio::sync::Notify::new(),
            release: tokio::sync::Notify::new(),
        });
        let audit = Arc::new(CollectingAudit::default());
        let handlers = Arc::new(test_handlers(gate.clone(), audit.clone(), backend));
        let (tx, sub) = fed_subscription();
        let (stop_tx, stop_rx) = shutdown_pair();
        let serve = tokio::spawn(serve_namespace(
            client,
            sub,
            "ssh-agent",
            stop_rx,
            handlers,
            silent_log(),
        ));
        let old = p.epoch();
        let mut sign = sign_env(&b64(&pub_bytes), &b64(&git_login()), 0);
        sign.id = "running".into();
        tx.send(on_stream(sign, old)).await.unwrap();
        gate.asked.notified().await;
        tx.send(on_stream(list_request_id("queued"), old))
            .await
            .unwrap();
        // The app starts and quits again while the person is approving.
        p.set(true);
        p.set(false);
        eventually(
            "the queue drained at the yield, the handler still running",
            || tx.capacity() == ENVELOPE_CHANNEL_CAP,
        )
        .await;
        gate.release.notify_one();
        eventually("the running handler's audit", || {
            !audit_entries(&audit).is_empty()
        })
        .await;
        // The daemon's new stream: the server delivers the still-pending request again.
        tx.send(on_stream(list_request_id("queued"), p.epoch()))
            .await
            .unwrap();
        eventually("the re-delivered request answered", || {
            respond_posts(&srv) == 1 && audit_entries(&audit).len() == 2
        })
        .await;
        tokio::time::sleep(Duration::from_millis(100)).await;
        assert_eq!(respond_posts(&srv), 1, "only the re-delivered request");
        let entries = audit_entries(&audit);
        assert_eq!(entries.len(), 2, "the old copy ran no handler: {entries:?}");
        assert_eq!(entries[0].result, AUDIT_RESULT_UNDELIVERED, "{entries:?}");
        assert!(
            entries[0].reason.contains(AUDIT_NOTE_LEFT_FOR_APP),
            "{entries:?}"
        );
        assert_ne!(entries[1].result, AUDIT_RESULT_UNDELIVERED, "{entries:?}");
        let _ = stop_tx.send(true);
        let _ = serve.await;
    }

    /// Presence that never changes leaves everything as before: a `503` retry is sent, and
    /// a request queued behind it is handled and answered.
    #[tokio::test]
    async fn presence_that_never_changes_leaves_retries_and_the_queue_alone() {
        let srv = ScriptedServer::spawn(
            vec![Answer(
                503,
                vec![("Retry-After", "1".into())],
                api_error("RESPONSE_NOT_DELIVERED"),
            )],
            Answer(204, vec![], String::new()),
        )
        .await;
        let p = FakePresence::new(false);
        let client = open_client(&srv.url())
            .with_presence(Some(p.rx()))
            .with_test_respond_timing(Duration::from_millis(800), Duration::from_secs(5));
        let audit = Arc::new(CollectingAudit::default());
        let handlers = Arc::new(test_handlers(
            approve_gate(),
            audit.clone(),
            empty_backend(),
        ));
        let (tx, sub) = fed_subscription();
        let (stop_tx, stop_rx) = shutdown_pair();
        let serve = tokio::spawn(serve_namespace(
            client,
            sub,
            "ssh-agent",
            stop_rx,
            handlers,
            silent_log(),
        ));
        tx.send(on_stream(list_request_id("first"), p.epoch()))
            .await
            .unwrap();
        eventually("the first POST", || respond_posts(&srv) == 1).await;
        tx.send(on_stream(list_request_id("second"), p.epoch()))
            .await
            .unwrap();
        eventually("both answered", || {
            respond_posts(&srv) == 3 && audit_entries(&audit).len() == 2
        })
        .await;
        for e in audit_entries(&audit) {
            assert_ne!(e.result, AUDIT_RESULT_UNDELIVERED, "{e:?}");
            assert!(!e.reason.contains(AUDIT_NOTE_LEFT_FOR_APP), "{e:?}");
        }
        assert_eq!(p.epoch(), 0);
        let _ = stop_tx.send(true);
        let _ = serve.await;
    }

    /// A stream opened before the app started and quit again, both between two looks at
    /// the channel (`present` never seen), is closed and opened again under the new epoch:
    /// only a new stream is delivered again what is pending on the server.
    #[tokio::test(start_paused = true)]
    async fn a_stream_from_before_a_start_and_quit_is_opened_again() {
        let bus = SimBus::new();
        let p = FakePresence::new(false);
        let daemon = bus.client("daemon").with_presence(Some(p.rx()));
        let (_d_tx, _d_subs) = subscribe_all(&daemon);
        run_for(Duration::from_secs(5)).await;
        let t0 = tokio::time::Instant::now();
        for ns in PIN_NS {
            assert_eq!(bus.holder(ns).as_deref(), Some("daemon"));
        }
        // Started and gone again before the subscription task runs.
        p.set(true);
        p.set(false);
        run_for(Duration::from_secs(5)).await;
        for ns in PIN_NS {
            let opened = bus
                .attempts("daemon", t0)
                .iter()
                .filter(|e| e.ns == ns && e.ok)
                .count();
            assert_eq!(opened, 1, "{ns}: one new stream");
            assert_eq!(bus.holder(ns).as_deref(), Some("daemon"), "{ns}");
        }
    }
}
