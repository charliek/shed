//! The desktop approval channel — the stateful async UDS server, ported from the
//! Go daemon's `desktop_server.go` / `desktop_gate.go` (catalog §8). It exposes
//! the all-namespace audit/event stream plus the approval request/response and
//! `token.get`/`token.response` channels to a single active consumer (normally the
//! shed-desktop app). Wire shapes + framing/correlation come from
//! `crate::desktop_protocol` (the server direction of the shed-host-agent codec;
//! the client direction lives in `shed_core::approval::protocol`).
//!
//! **Single consumer, last-writer-wins.** A second connection supersedes the
//! first (the old one gets `hello_ack{accepted:false, reason:"superseded"}` then is
//! closed). **Fail-closed:** with no consumer connected — or on timeout, disconnect,
//! or a transport error — an approval denies, and `token.get` returns an error with
//! no partial token. Trust is filesystem perms only (`0700` dir / `0600` socket);
//! there is NO peer-UID check, matching the Go server.
//!
//! Feature-gated behind `desktop-forwarding` (the headless daemon skips it).

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::unix::{OwnedReadHalf, OwnedWriteHalf};
use tokio::net::{UnixListener, UnixStream};
use tokio::sync::{mpsc, oneshot};

use crate::desktop_protocol::{
    self as protocol, ApprovalResponseMsg, AuditEntryView, ClientInfo, DesktopInbound, TokenGetMsg,
};

use crate::approval::{ApprovalGate, ApprovalOutcome};
use crate::config::{NS_AWS_CREDENTIALS, NS_DOCKER_CREDENTIALS, NS_SSH_AGENT, POLICY_SHED_DESKTOP};
use crate::status::{now_rfc3339, now_unix, rfc3339_utc};

/// 1 MiB per-line cap on inbound frames — a larger frame is a protocol violation
/// (disconnect), never unbounded memory growth. Matches Go's `maxFrameBytes`.
const MAX_FRAME_BYTES: usize = 1 << 20;
/// Per-frame write deadline; a connected-but-not-reading app can't block a send
/// forever. Matches Go's `consumerWriteTimeout`.
const CONSUMER_WRITE_TIMEOUT: Duration = Duration::from_secs(5);
/// Server->app keepalive interval. Matches the Go 10s ping ticker.
const PING_INTERVAL: Duration = Duration::from_secs(10);
/// A new connection must send its `hello` within this grace period. Matches the Go
/// 2s first-line read deadline.
const HELLO_DEADLINE: Duration = Duration::from_secs(2);
/// Replay ring capacity (last N audit events). Matches Go's `ringMax`. (Used by
/// `publish_audit`, which has no bus/backend caller until slice 1b/1c.)
#[allow(dead_code)]
const RING_MAX: usize = 100;
/// Bounded outbound-writer queue depth. The Go server has NO app-level queue — it
/// writes each frame synchronously under a write mutex with the 5s deadline, so a
/// slow reader applies backpressure at the socket. This Rust port hands frames to
/// a background writer task, so the channel needs an explicit bound to reproduce
/// that backpressure: a slow/stuck reader can't let queued events/tokens/approvals
/// grow without limit. Sized comfortably above `RING_MAX` so a legitimate replay
/// burst (up to `RING_MAX` buffered events pushed back-to-back) never spuriously
/// overflows; a genuine overflow means the reader has stalled and is treated as a
/// transport failure (demote + fail-close pending, same path as a write-deadline).
const WRITER_QUEUE_CAP: usize = 256;
/// Fallback when the configured approval timeout is non-positive. Matches
/// `NewDesktopServer`'s guard.
const DEFAULT_APPROVAL_TIMEOUT: Duration = Duration::from_secs(25);
/// The egress audit namespace advertised in `hello_ack.namespaces` (Go
/// `namespaceEgress`).
const NAMESPACE_EGRESS: &str = "egress";

// ---------------------------------------------------------------------------
// Control-token minter seam
// ---------------------------------------------------------------------------

/// A minted control-scoped token: the token plus an optional RFC3339 expiry
/// (`None` = non-expiring / unknown, which omits `expires_at` in the reply).
#[derive(Debug)]
pub struct MintedControlToken {
    pub token: String,
    pub expires_at: Option<String>,
}

/// Mints CONTROL-scoped tokens on the app's behalf (answers `token.get`). The real
/// SSH-bootstrap minter is a later slice; this is the injection seam.
#[async_trait::async_trait]
pub trait ControlTokenMinter: Send + Sync {
    /// Mint a control token for `server`. `Err(msg)` fails the `token.get` closed —
    /// the reply carries the message and no token.
    async fn mint_control(&self, server: &str) -> Result<MintedControlToken, String>;
}

/// A stand-in minter that returns a canned token — used only by this module's tests to
/// drive `token.get` without the real SSH-bootstrap minter. Production wires the real
/// `controltoken::ControlTokenProvider` (`main.rs`).
#[cfg(test)]
pub struct StubControlMinter;

#[cfg(test)]
#[async_trait::async_trait]
impl ControlTokenMinter for StubControlMinter {
    async fn mint_control(&self, _server: &str) -> Result<MintedControlToken, String> {
        Ok(MintedControlToken {
            token: "stub-control-token".to_string(),
            expires_at: None,
        })
    }
}

// ---------------------------------------------------------------------------
// Approval outcome
// ---------------------------------------------------------------------------

// The delegated-approval outcome is the crate-wide `ApprovalOutcome` (imported
// above) so a single shape flows from `request_approval` through the gate seam to
// the audit entry. On a received decision (approve OR deny), `decided_by` defaults
// to `"user"` when the app left it empty; on a no-decision fail-closed (no
// consumer, timeout, disconnect, transport error) `decided_by` is empty (matching
// Go's empty `ApprovalOutcome{}` on the error path).

/// The app's decision as delivered internally (raw, before the `decided_by`
/// default is applied). Mirrors Go's `desktopDecision`.
struct DesktopDecision {
    approved: bool,
    decided_by: String,
    scope: Option<String>,
    ttl: Option<String>,
}

fn decision_to_outcome(dec: DesktopDecision) -> ApprovalOutcome {
    let decided_by = if dec.decided_by.is_empty() {
        "user".to_string()
    } else {
        dec.decided_by
    };
    ApprovalOutcome {
        approved: dec.approved,
        decided_by,
        scope: dec.scope,
        ttl: dec.ttl,
    }
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

/// The single active consumer (normally shed-desktop).
struct Consumer {
    /// Monotonic connection id — the ownership token that a superseded connection
    /// can't forge (mirrors Go's `*consumerConn` pointer identity).
    id: u64,
    /// Self-reported client identity from the hello (surfaced in status).
    client: ClientInfo,
    /// Send onto this to write a frame to the consumer. Bounded (`WRITER_QUEUE_CAP`)
    /// so a stalled reader applies backpressure instead of unbounded queue growth.
    writer: mpsc::Sender<Vec<u8>>,
    /// Fire to signal this connection's read loop to stop (used on supersede).
    close: oneshot::Sender<()>,
}

/// An in-flight approval, bound to the connection that owns it so a superseded
/// connection can't resolve a request it merely observed.
struct Pending {
    tx: oneshot::Sender<DesktopDecision>,
    owner: u64,
}

struct Inner {
    consumer: Option<Consumer>,
    pending: HashMap<String, Pending>,
    /// Serialized `event` frames (last `RING_MAX`) for replay. Storing the exact
    /// bytes keeps a replayed frame's `id` stable (matches Go, which re-sends the
    /// same `eventMsg`).
    ring: Vec<String>,
}

/// The desktop approval channel server. Construct with [`DesktopServer::new`],
/// then [`serve`](DesktopServer::serve) on a bound listener.
pub struct DesktopServer {
    agent_version: String,
    gate_namespaces: Vec<String>,
    timeout: Duration,
    minter: Option<Arc<dyn ControlTokenMinter>>,
    conn_counter: AtomicU64,
    inner: Mutex<Inner>,
}

impl DesktopServer {
    /// Build a server. `approval_timeout` bounds each delegated approval before a
    /// fail-closed deny (a non-positive value falls back to 25s, matching
    /// `NewDesktopServer`). `gate_namespaces` are advertised in `hello_ack`. A
    /// `None` minter makes `token.get` fail closed.
    pub fn new(
        agent_version: String,
        gate_namespaces: Vec<String>,
        approval_timeout: Duration,
        minter: Option<Arc<dyn ControlTokenMinter>>,
    ) -> Arc<Self> {
        let timeout = if approval_timeout.is_zero() {
            DEFAULT_APPROVAL_TIMEOUT
        } else {
            approval_timeout
        };
        Arc::new(Self {
            agent_version,
            gate_namespaces,
            timeout,
            minter,
            conn_counter: AtomicU64::new(0),
            inner: Mutex::new(Inner {
                consumer: None,
                pending: HashMap::new(),
                ring: Vec::new(),
            }),
        })
    }

    /// Accept connections until `shutdown` resolves, then unlink the socket file so
    /// a restart can rebind (tokio's `UnixListener` doesn't unlink on drop — matches
    /// `serve_status_socket` and the Go daemon's `ln.Close()`).
    pub async fn serve<F>(self: Arc<Self>, listener: UnixListener, path: PathBuf, shutdown: F)
    where
        F: std::future::Future<Output = ()>,
    {
        tokio::pin!(shutdown);
        loop {
            tokio::select! {
                _ = &mut shutdown => break,
                accepted = listener.accept() => {
                    match accepted {
                        Ok((stream, _addr)) => {
                            let id = self.conn_counter.fetch_add(1, Ordering::Relaxed);
                            let this = Arc::clone(&self);
                            tokio::spawn(async move { this.handle_conn(stream, id).await });
                        }
                        Err(_) => break, // listener closed / fatal accept error
                    }
                }
            }
        }
        let _ = std::fs::remove_file(&path);
    }

    /// Send an approval request to the connected app and block on the reply within
    /// the timeout. Fail-closed: denies (with an empty `decided_by`) when no app is
    /// connected, on timeout, on disconnect, or on a transport error. Folds the Go
    /// `DesktopServer.RequestApproval` + `desktopGate.Approve` into one call.
    pub async fn request_approval(
        &self,
        namespace: &str,
        op: &str,
        server: &str,
        shed: &str,
        detail: &str,
    ) -> ApprovalOutcome {
        let id = new_id();
        let (tx, rx) = oneshot::channel();
        let (owner, writer) = {
            let mut inner = self.inner.lock().unwrap();
            let Some(consumer) = inner.consumer.as_ref() else {
                return ApprovalOutcome::denied_no_decision(); // no consumer -> fail closed
            };
            let owner = consumer.id;
            let writer = consumer.writer.clone();
            // Register BEFORE writing so a fast reply can't race ahead of registration.
            inner.pending.insert(id.clone(), Pending { tx, owner });
            (owner, writer)
        };

        let expires_at = rfc3339_utc(now_unix() + self.timeout.as_secs() as i64);
        let server_opt = if server.is_empty() {
            None
        } else {
            Some(server)
        };
        let frame = protocol::approval_request(
            &id,
            &now_rfc3339(),
            namespace,
            op,
            server_opt,
            shed,
            detail,
            &expires_at,
        );
        if writer.try_send(with_newline(frame)).is_err() {
            // Transport gone (closed) or the bounded queue is full (a stalled
            // reader). Either way fail closed AND demote — clearing the consumer and
            // fail-closing every pending owned by it (including this one), matching
            // the Go synchronous send that errors on a full/dead socket.
            self.demote(owner);
            return ApprovalOutcome::denied_no_decision();
        }

        let outcome = tokio::select! {
            res = rx => match res {
                Ok(dec) => decision_to_outcome(dec),
                Err(_) => ApprovalOutcome::denied_no_decision(), // sender dropped without a decision
            },
            _ = tokio::time::sleep(self.timeout) => ApprovalOutcome::denied_no_decision(),
        };
        // Idempotent cleanup (resolve/demote may already have removed it).
        self.inner.lock().unwrap().pending.remove(&id);
        outcome
    }

    /// Map an audit entry to an `event` frame, append it to the replay ring, and
    /// send it to the active consumer (if any). Non-blocking. Mirrors the Go
    /// `forwardAudit` fan-out. No bus/backend caller until slice 1b/1c.
    #[allow(dead_code)]
    pub fn publish_audit(&self, entry: &AuditEntryView) {
        let frame = protocol::event(&new_id(), &entry.ts, entry);
        // Enqueue under the lock (a bounded `try_send` never awaits). On overflow /
        // closed the reader has stalled — report the owning conn so we can demote it
        // (fail-closing its pending) outside the lock.
        let failed_conn = {
            let mut inner = self.inner.lock().unwrap();
            inner.ring.push(frame.clone());
            let overflow = inner.ring.len().saturating_sub(RING_MAX);
            if overflow > 0 {
                inner.ring.drain(0..overflow);
            }
            match inner.consumer.as_ref() {
                Some(c) => match c.writer.try_send(with_newline(frame)) {
                    Ok(()) => None,
                    Err(_) => Some(c.id), // queue full / closed -> stalled reader
                },
                None => None,
            }
        };
        if let Some(conn_id) = failed_conn {
            self.demote(conn_id);
        }
    }

    /// The connected consumer's self-reported `(name, version)`, or `None` when no
    /// app is connected. Feeds the status report's approval channel.
    pub fn consumer_info(&self) -> Option<(String, String)> {
        let inner = self.inner.lock().unwrap();
        inner
            .consumer
            .as_ref()
            .map(|c| (c.client.name.clone(), c.client.version.clone()))
    }

    async fn handle_conn(self: Arc<Self>, stream: UnixStream, conn_id: u64) {
        let (read_half, mut write_half) = stream.into_split();
        let mut reader = BufReader::new(read_half);
        let mut line = Vec::new();

        // Require a hello within the grace period; a non-hello first line drops.
        let first = tokio::time::timeout(
            HELLO_DEADLINE,
            read_frame_capped(&mut reader, &mut line, MAX_FRAME_BYTES),
        )
        .await;
        if !matches!(first, Ok(Ok(true))) {
            return;
        }
        let hello = match protocol::decode_desktop(strip_trailing_newline(&line)) {
            Ok(DesktopInbound::Hello(h)) => h,
            _ => return,
        };

        // Send hello_ack BEFORE promoting so an approval can't route here mid-handshake.
        let namespaces = [
            NS_SSH_AGENT,
            NS_AWS_CREDENTIALS,
            NS_DOCKER_CREDENTIALS,
            NAMESPACE_EGRESS,
        ];
        let ack = protocol::hello_ack(
            &new_id(),
            &now_rfc3339(),
            &self.agent_version,
            &namespaces,
            &self.gate_namespaces,
            self.timeout.as_millis() as i64,
            true,
            None,
        );
        if !write_first_frame(&mut write_half, &with_newline(ack)).await {
            return; // hello_ack write failed -> never promote
        }

        // The writer task owns the write half from here (5s per-frame deadline). The
        // queue is bounded (`WRITER_QUEUE_CAP`) so a stalled reader applies
        // backpressure instead of unbounded growth (see the const).
        let (writer_tx, writer_rx) = mpsc::channel::<Vec<u8>>(WRITER_QUEUE_CAP);
        let mut writer_task = tokio::spawn(writer_loop(write_half, writer_rx));

        let (close_tx, mut close_rx) = oneshot::channel::<()>();
        self.promote(conn_id, hello.client.clone(), writer_tx.clone(), close_tx);
        self.replay(&writer_tx, hello.replay_events);

        let ping_task = tokio::spawn(ping_loop(writer_tx.clone()));

        loop {
            line.clear();
            tokio::select! {
                biased;
                _ = &mut close_rx => break, // superseded by a newer connection
                // The writer task only exits here on a transport failure (a write
                // past the 5s deadline or an io error) — during the read loop the
                // channel still has live senders, so a clean drain-to-empty can't
                // happen. Break so cleanup demotes and fail-closes in-flight
                // approvals now (on the write deadline), not after the full timeout.
                _ = &mut writer_task => break,
                res = read_frame_capped(&mut reader, &mut line, MAX_FRAME_BYTES) => {
                    if !matches!(res, Ok(true)) {
                        break; // EOF / over-cap / io error -> disconnect
                    }
                }
            }
            let trimmed = strip_trailing_newline(&line);
            if trimmed.is_empty() {
                continue;
            }
            match protocol::decode_desktop(trimmed) {
                Ok(DesktopInbound::ApprovalResponse(resp)) => {
                    let request_id = resp.request_id.clone();
                    self.resolve(&request_id, decision_from_response(resp), conn_id);
                }
                Ok(DesktopInbound::TokenGet(req)) => {
                    // Mint in its own task: a bootstrap is a bounded round-trip and
                    // must not stall this read loop (and thus approvals). The task is
                    // bound to THIS connection id (not a captured writer) so a token
                    // minted after a supersede is dropped, never written to the
                    // superseded connection (Go closes the old conn on promote).
                    let this = Arc::clone(&self);
                    tokio::spawn(async move { this.handle_token_get(conn_id, req).await });
                }
                Ok(DesktopInbound::Pong) => {} // liveness only
                Ok(DesktopInbound::Hello(_)) | Ok(DesktopInbound::Unknown { .. }) => {}
                Err(_) => {} // malformed line -> skip (matches Go's continue)
            }
        }

        // Cleanup: stop pings, then demote FIRST — that clears the active consumer
        // (dropping the shared writer clone) and fail-closes every in-flight approval
        // immediately, so a disconnect resolves them without waiting on the writer.
        // Only then drop our writer clone + let the writer drain any queued frame
        // (e.g. a superseded ack) within a bounded window before it exits.
        ping_task.abort();
        self.demote(conn_id);
        drop(writer_tx);
        // If the loop broke because the writer task already exited (transport
        // failure), it's done — don't re-await the handle (that would panic). Only a
        // still-running writer (supersede / EOF path) gets the bounded drain window
        // to flush a queued frame such as the superseded ack.
        if !writer_task.is_finished()
            && tokio::time::timeout(
                CONSUMER_WRITE_TIMEOUT + Duration::from_secs(1),
                &mut writer_task,
            )
            .await
            .is_err()
        {
            writer_task.abort();
        }
    }

    /// Answer a `token.get`: mint a control token for the requested server and reply
    /// with `token.response`. Fail-closed — on any error `error` is set and
    /// `token`/`expires_at` stay empty (never a partial token).
    ///
    /// Bound to `conn_id`: minting is an async round-trip, so a newer connection may
    /// supersede this one before it completes. The reply is delivered ONLY if this
    /// connection is still the active consumer at send time (checked + enqueued
    /// atomically under the lock). Otherwise the minted CONTROL token is dropped — it
    /// must never reach a superseded connection (Go closes the old conn on promote).
    async fn handle_token_get(&self, conn_id: u64, req: TokenGetMsg) {
        let (token, expires_at, error): (Option<String>, Option<String>, Option<String>) =
            match &self.minter {
                None => (
                    None,
                    None,
                    Some("control-token minting is not available".to_string()),
                ),
                Some(minter) => match minter.mint_control(&req.server).await {
                    Ok(minted) => (Some(minted.token), minted.expires_at, None),
                    Err(msg) => (None, None, Some(msg)),
                },
            };
        let frame = protocol::token_response(
            &new_id(),
            &now_rfc3339(),
            &req.id,
            &req.server,
            token.as_deref(),
            expires_at.as_deref(),
            error.as_deref(),
        );
        // Enqueue under the lock so the "still the active consumer?" check and the
        // send are atomic w.r.t. `promote`'s swap. `try_send` never awaits.
        let send_result = {
            let inner = self.inner.lock().unwrap();
            match inner.consumer.as_ref() {
                Some(c) if c.id == conn_id => Some(c.writer.try_send(with_newline(frame))),
                _ => None, // superseded / gone -> never write a token to a stale conn
            }
        };
        if let Some(Err(_)) = send_result {
            self.demote(conn_id); // queue full / closed -> stalled reader, fail closed
        }
    }

    fn promote(
        &self,
        conn_id: u64,
        client: ClientInfo,
        writer: mpsc::Sender<Vec<u8>>,
        close: oneshot::Sender<()>,
    ) {
        let old = {
            let mut inner = self.inner.lock().unwrap();
            inner.consumer.replace(Consumer {
                id: conn_id,
                client,
                writer,
                close,
            })
        };
        // Last-writer-wins: tell the superseded consumer + close it (outside the lock).
        if let Some(old) = old {
            if old.id != conn_id {
                let Consumer {
                    writer: old_writer,
                    close: old_close,
                    ..
                } = old;
                let ack = protocol::hello_ack(
                    &new_id(),
                    &now_rfc3339(),
                    &self.agent_version,
                    &[],
                    &[],
                    0,
                    false,
                    Some("superseded"),
                );
                let _ = old_writer.try_send(with_newline(ack));
                let _ = old_close.send(());
            }
        }
    }

    fn demote(&self, conn_id: u64) {
        let mut inner = self.inner.lock().unwrap();
        if inner.consumer.as_ref().map(|c| c.id) == Some(conn_id) {
            inner.consumer = None;
        }
        // Fail-closed every in-flight approval owned by this connection, whether it
        // is still the active consumer or was already superseded.
        let owned: Vec<String> = inner
            .pending
            .iter()
            .filter(|(_, p)| p.owner == conn_id)
            .map(|(id, _)| id.clone())
            .collect();
        for id in owned {
            if let Some(pending) = inner.pending.remove(&id) {
                let _ = pending.tx.send(DesktopDecision {
                    approved: false,
                    decided_by: String::new(),
                    scope: None,
                    ttl: None,
                });
            }
        }
    }

    fn resolve(&self, request_id: &str, dec: DesktopDecision, from_conn: u64) {
        let mut inner = self.inner.lock().unwrap();
        // Ownership guard: only the consumer the request was sent to may resolve it.
        let owns = inner
            .pending
            .get(request_id)
            .map(|p| p.owner == from_conn)
            .unwrap_or(false);
        let pending = if owns {
            inner.pending.remove(request_id)
        } else {
            None
        };
        drop(inner);
        if let Some(pending) = pending {
            let _ = pending.tx.send(dec);
        }
    }

    fn replay(&self, writer: &mpsc::Sender<Vec<u8>>, n: i64) {
        if n <= 0 {
            return;
        }
        let frames = {
            let inner = self.inner.lock().unwrap();
            let start = inner.ring.len().saturating_sub(n as usize);
            inner.ring[start..].to_vec()
        };
        // Best-effort (matches Go's replay, which ignores per-frame send errors). The
        // burst is bounded by `RING_MAX` < `WRITER_QUEUE_CAP`, so a freshly-promoted
        // healthy consumer never overflows on replay alone.
        for frame in frames {
            let _ = writer.try_send(with_newline(frame));
        }
    }
}

/// The shed-desktop approval gate: delegates a credential op's decision to the
/// connected app over the UDS. A Rust port of Go's `desktopGate` — it holds an
/// `Arc<DesktopServer>` and forwards `approve` to [`DesktopServer::request_approval`],
/// which fails closed (denies) when no app is connected, on timeout, on disconnect,
/// or on a transport error. Lives here (behind `desktop-forwarding`) because it is
/// the only gate that touches the desktop server; the bus/ssh handler sees only the
/// `ApprovalGate` trait.
pub struct DesktopGate {
    server: Arc<DesktopServer>,
}

impl DesktopGate {
    pub fn new(server: Arc<DesktopServer>) -> DesktopGate {
        DesktopGate { server }
    }
}

#[async_trait::async_trait]
impl ApprovalGate for DesktopGate {
    async fn approve(
        &self,
        ns: &str,
        op: &str,
        server: &str,
        shed: &str,
        detail: &str,
    ) -> ApprovalOutcome {
        // request_approval already applies the `decided_by` default + the
        // no-consumer/timeout/disconnect fail-closed, returning the outcome on both
        // approve and deny (so a denied op is audited with its decided_by/scope/ttl).
        self.server
            .request_approval(ns, op, server, shed, detail)
            .await
    }
    fn method(&self) -> &str {
        POLICY_SHED_DESKTOP
    }
}

fn decision_from_response(resp: ApprovalResponseMsg) -> DesktopDecision {
    DesktopDecision {
        approved: resp.decision == "approve",
        decided_by: resp.decided_by,
        scope: resp.scope,
        ttl: resp.ttl,
    }
}

async fn writer_loop(mut write_half: OwnedWriteHalf, mut rx: mpsc::Receiver<Vec<u8>>) {
    while let Some(bytes) = rx.recv().await {
        match tokio::time::timeout(CONSUMER_WRITE_TIMEOUT, write_half.write_all(&bytes)).await {
            Ok(Ok(())) => {}
            // A write past the deadline or an io error drops the connection (fail
            // closed): returning drops `rx` + the write half (closing the socket), and
            // the read loop — which watches this task — then demotes and fail-closes
            // every in-flight approval owned by this connection.
            _ => return,
        }
    }
}

async fn ping_loop(writer: mpsc::Sender<Vec<u8>>) {
    let mut ticker = tokio::time::interval(PING_INTERVAL);
    // tokio's interval fires immediately on the first tick; Go's ticker fires only
    // after the period, so consume the first tick to match (first ping at +10s).
    ticker.tick().await;
    loop {
        ticker.tick().await;
        let frame = protocol::ping(&new_id(), &now_rfc3339());
        if writer.try_send(with_newline(frame)).is_err() {
            return; // consumer gone or the queue is backed up (writer will time out)
        }
    }
}

/// Write a single frame with the per-frame deadline (used for the pre-promote
/// hello_ack, before the writer task owns the write half). `true` on success.
async fn write_first_frame(write_half: &mut OwnedWriteHalf, bytes: &[u8]) -> bool {
    matches!(
        tokio::time::timeout(CONSUMER_WRITE_TIMEOUT, write_half.write_all(bytes)).await,
        Ok(Ok(()))
    )
}

fn new_id() -> String {
    uuid::Uuid::new_v4().to_string()
}

fn with_newline(line: String) -> Vec<u8> {
    let mut bytes = line.into_bytes();
    bytes.push(b'\n');
    bytes
}

fn strip_trailing_newline(line: &[u8]) -> &[u8] {
    let mut end = line.len();
    while end > 0 && (line[end - 1] == b'\n' || line[end - 1] == b'\r') {
        end -= 1;
    }
    &line[..end]
}

/// Read one newline-terminated frame into `buf` (including the trailing `\n`),
/// capped at `max` bytes. `Ok(true)` = a complete frame; `Ok(false)` = EOF (any
/// partial bytes are dropped, never processed); `Err` on an io error or a frame
/// exceeding `max` (a protocol violation -> disconnect). Mirrors the client's
/// `read_frame_capped`.
async fn read_frame_capped(
    reader: &mut BufReader<OwnedReadHalf>,
    buf: &mut Vec<u8>,
    max: usize,
) -> std::io::Result<bool> {
    loop {
        let (found, take) = {
            let available = reader.fill_buf().await?;
            if available.is_empty() {
                return Ok(false); // EOF -> drop any partial, signal disconnect
            }
            match available.iter().position(|&b| b == b'\n') {
                Some(pos) => {
                    buf.extend_from_slice(&available[..=pos]);
                    (true, pos + 1)
                }
                None => {
                    buf.extend_from_slice(available);
                    (false, available.len())
                }
            }
        };
        reader.consume(take);
        // Check the cap after EVERY extend, including the newline-found branch: a
        // final chunk that carries the newline can itself push `buf` over the cap, and
        // a slightly-over-limit frame must still disconnect (matches Go's `Scanner`
        // token cap), never be accepted just because it happened to end in '\n'.
        if buf.len() > max {
            return Err(std::io::Error::new(
                std::io::ErrorKind::InvalidData,
                "desktop frame exceeded the size cap",
            ));
        }
        if found {
            return Ok(true);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::{json, Value};
    use std::path::Path;

    async fn wait_until(mut cond: impl FnMut() -> bool) -> bool {
        for _ in 0..300 {
            if cond() {
                return true;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        false
    }

    /// A bound + serving `DesktopServer` plus its socket path. The serve task runs
    /// with a never-resolving shutdown; the socket is removed on drop.
    struct Harness {
        server: Arc<DesktopServer>,
        path: PathBuf,
    }

    impl Harness {
        fn start(
            minter: Option<Arc<dyn ControlTokenMinter>>,
            gate: Vec<String>,
            timeout: Duration,
        ) -> Self {
            let path = std::env::temp_dir().join(format!("shed-ds-test-{}.sock", new_id()));
            let _ = std::fs::remove_file(&path);
            let listener = UnixListener::bind(&path).unwrap();
            let server = DesktopServer::new("v-test".into(), gate, timeout, minter);
            let serve = server.clone();
            let serve_path = path.clone();
            tokio::spawn(async move {
                serve
                    .serve(listener, serve_path, std::future::pending::<()>())
                    .await
            });
            Harness { server, path }
        }
    }

    impl Drop for Harness {
        fn drop(&mut self) {
            let _ = std::fs::remove_file(&self.path);
        }
    }

    /// A fake shed-desktop app connection.
    struct TestApp {
        reader: BufReader<OwnedReadHalf>,
        write_half: OwnedWriteHalf,
    }

    impl TestApp {
        async fn connect(path: &Path) -> Self {
            let (r, w) = UnixStream::connect(path).await.unwrap().into_split();
            TestApp {
                reader: BufReader::new(r),
                write_half: w,
            }
        }

        async fn send(&mut self, value: Value) {
            let mut bytes = serde_json::to_vec(&value).unwrap();
            bytes.push(b'\n');
            self.write_half.write_all(&bytes).await.unwrap();
        }

        async fn recv(&mut self) -> Value {
            let mut line = Vec::new();
            let n = self.reader.read_until(b'\n', &mut line).await.unwrap();
            assert!(n > 0, "unexpected EOF from desktop server");
            serde_json::from_slice(&line).unwrap()
        }

        async fn handshake(&mut self, replay_events: i64) -> Value {
            self.send(json!({
                "type": "hello",
                "client": {"name": "App", "version": "9", "pid": 7},
                "replay_events": replay_events,
            }))
            .await;
            let ack = self.recv().await;
            assert_eq!(ack["type"], "hello_ack");
            ack
        }
    }

    #[tokio::test]
    async fn handshake_acks_and_registers_consumer() {
        let h = Harness::start(None, vec!["ssh-agent".into()], Duration::from_secs(25));
        let mut app = TestApp::connect(&h.path).await;
        let ack = app.handshake(0).await;
        assert_eq!(ack["accepted"], true);
        assert_eq!(ack["agent"]["approval_method"], "shed-desktop");
        assert_eq!(
            ack["namespaces"],
            json!([
                "ssh-agent",
                "aws-credentials",
                "docker-credentials",
                "egress"
            ])
        );
        assert_eq!(ack["gate_namespaces"], json!(["ssh-agent"]));
        assert_eq!(ack["request_timeout_ms"], 25000);

        let server = h.server.clone();
        assert!(wait_until(move || server.consumer_info().is_some()).await);
        assert_eq!(h.server.consumer_info(), Some(("App".into(), "9".into())));
    }

    #[tokio::test]
    async fn non_hello_first_line_is_dropped() {
        let h = Harness::start(None, vec![], Duration::from_secs(25));
        let mut app = TestApp::connect(&h.path).await;
        app.send(json!({"type": "pong"})).await; // not a hello
        let mut line = Vec::new();
        // The server drops the connection -> our read hits EOF (0 bytes).
        let n = app.reader.read_until(b'\n', &mut line).await.unwrap();
        assert_eq!(n, 0, "expected the server to close a non-hello connection");
        assert!(h.server.consumer_info().is_none());
    }

    #[tokio::test]
    async fn approval_approved_returns_outcome() {
        let h = Harness::start(None, vec![], Duration::from_secs(25));
        let mut app = TestApp::connect(&h.path).await;
        app.handshake(0).await;
        let ready = h.server.clone();
        assert!(wait_until(move || ready.consumer_info().is_some()).await);

        let server = h.server.clone();
        let req = tokio::spawn(async move {
            server
                .request_approval("ssh-agent", "sign", "mini2", "web", "ed25519")
                .await
        });
        let ar = app.recv().await;
        assert_eq!(ar["type"], "approval_request");
        assert_eq!(ar["namespace"], "ssh-agent");
        assert_eq!(ar["op"], "sign");
        assert_eq!(ar["server"], "mini2");
        assert_eq!(ar["shed"], "web");
        let rid = ar["id"].as_str().unwrap().to_string();
        app.send(json!({
            "type": "approval_response", "request_id": rid,
            "decision": "approve", "decided_by": "touchid",
            "scope": "per-session", "ttl": "1h",
        }))
        .await;

        let outcome = tokio::time::timeout(Duration::from_secs(5), req)
            .await
            .unwrap()
            .unwrap();
        assert!(outcome.approved);
        assert_eq!(outcome.decided_by, "touchid");
        assert_eq!(outcome.scope.as_deref(), Some("per-session"));
        assert_eq!(outcome.ttl.as_deref(), Some("1h"));
    }

    #[tokio::test]
    async fn approval_denied_defaults_decided_by_to_user() {
        let h = Harness::start(None, vec![], Duration::from_secs(25));
        let mut app = TestApp::connect(&h.path).await;
        app.handshake(0).await;
        let ready = h.server.clone();
        assert!(wait_until(move || ready.consumer_info().is_some()).await);

        let server = h.server.clone();
        let req = tokio::spawn(async move {
            server
                .request_approval("ssh-agent", "sign", "", "web", "d")
                .await
        });
        let ar = app.recv().await;
        let rid = ar["id"].as_str().unwrap().to_string();
        // Deny with an empty decided_by -> defaults to "user".
        app.send(json!({"type": "approval_response", "request_id": rid, "decision": "deny", "decided_by": ""}))
            .await;
        let outcome = tokio::time::timeout(Duration::from_secs(5), req)
            .await
            .unwrap()
            .unwrap();
        assert!(!outcome.approved);
        assert_eq!(outcome.decided_by, "user");
    }

    #[tokio::test]
    async fn approval_without_consumer_denies_no_decision() {
        let h = Harness::start(None, vec![], Duration::from_secs(25));
        let outcome = h
            .server
            .request_approval("ssh-agent", "sign", "", "web", "d")
            .await;
        assert!(!outcome.approved);
        assert_eq!(outcome.decided_by, ""); // no-decision fail-closed
    }

    #[tokio::test]
    async fn approval_times_out_denies() {
        let h = Harness::start(None, vec![], Duration::from_millis(150));
        let mut app = TestApp::connect(&h.path).await;
        app.handshake(0).await;
        let ready = h.server.clone();
        assert!(wait_until(move || ready.consumer_info().is_some()).await);
        // The app never replies -> timeout -> deny.
        let outcome = h
            .server
            .request_approval("ssh-agent", "sign", "", "web", "d")
            .await;
        assert!(!outcome.approved);
        assert_eq!(outcome.decided_by, "");
        // Drain the request the server sent (keeps the socket tidy).
        let _ = app.recv().await;
    }

    #[tokio::test]
    async fn disconnect_fails_inflight_approval() {
        let h = Harness::start(None, vec![], Duration::from_secs(30));
        let mut app = TestApp::connect(&h.path).await;
        app.handshake(0).await;
        let ready = h.server.clone();
        assert!(wait_until(move || ready.consumer_info().is_some()).await);
        let server = h.server.clone();
        let req = tokio::spawn(async move {
            server
                .request_approval("ssh-agent", "sign", "", "web", "d")
                .await
        });
        // Wait for the request to be sent, then drop the connection.
        let _ = app.recv().await;
        drop(app);
        let outcome = tokio::time::timeout(Duration::from_secs(5), req)
            .await
            .unwrap()
            .unwrap();
        // Disconnect delivers a {approved:false} decision -> decided_by defaults "user".
        assert!(!outcome.approved);
        assert_eq!(outcome.decided_by, "user");
    }

    #[tokio::test]
    async fn second_consumer_supersedes_first() {
        let h = Harness::start(None, vec![], Duration::from_secs(25));
        let mut a1 = TestApp::connect(&h.path).await;
        a1.handshake(0).await;
        let ready = h.server.clone();
        assert!(wait_until(move || ready.consumer_info().is_some()).await);

        let mut a2 = TestApp::connect(&h.path).await;
        a2.handshake(0).await;

        // a1 receives a superseded ack.
        let msg = a1.recv().await;
        assert_eq!(msg["type"], "hello_ack");
        assert_eq!(msg["accepted"], false);
        assert_eq!(msg["reason"], "superseded");
        // a2 is now the active consumer.
        assert!(h.server.consumer_info().is_some());
    }

    #[tokio::test]
    async fn token_get_stub_minter_responds() {
        let h = Harness::start(
            Some(Arc::new(StubControlMinter)),
            vec![],
            Duration::from_secs(25),
        );
        let mut app = TestApp::connect(&h.path).await;
        app.handshake(0).await;
        app.send(json!({"type": "token.get", "id": "q1", "server": "mini2"}))
            .await;
        let resp = app.recv().await;
        assert_eq!(resp["type"], "token.response");
        assert_eq!(resp["in_reply_to"], "q1");
        assert_eq!(resp["server"], "mini2");
        assert_eq!(resp["token"], "stub-control-token");
        assert!(resp.get("error").is_none());
    }

    #[tokio::test]
    async fn token_get_without_minter_fails_closed() {
        let h = Harness::start(None, vec![], Duration::from_secs(25));
        let mut app = TestApp::connect(&h.path).await;
        app.handshake(0).await;
        app.send(json!({"type": "token.get", "id": "q1", "server": "mini2"}))
            .await;
        let resp = app.recv().await;
        assert_eq!(resp["error"], "control-token minting is not available");
        assert!(resp.get("token").is_none());
        assert!(resp.get("expires_at").is_none());
    }

    #[tokio::test]
    async fn audit_replays_buffered_and_forwards_live() {
        let h = Harness::start(None, vec![], Duration::from_secs(25));
        // Publish before any consumer -> buffered in the ring.
        h.server.publish_audit(&AuditEntryView {
            ts: "T1".into(),
            ns: "aws-credentials".into(),
            op: "get_credentials".into(),
            result: "ok".into(),
            ..Default::default()
        });

        let mut app = TestApp::connect(&h.path).await;
        app.handshake(10).await; // request replay of the last 10
        let ev = app.recv().await;
        assert_eq!(ev["type"], "event");
        assert_eq!(ev["kind"], "audit");
        assert_eq!(ev["op"], "get_credentials");
        assert_eq!(ev["ts"], "T1");

        let ready = h.server.clone();
        assert!(wait_until(move || ready.consumer_info().is_some()).await);
        // A live publish reaches the connected consumer.
        h.server.publish_audit(&AuditEntryView {
            ts: "T2".into(),
            ns: "ssh-agent".into(),
            op: "sign".into(),
            result: "ok".into(),
            ..Default::default()
        });
        let ev2 = app.recv().await;
        assert_eq!(ev2["op"], "sign");
        assert_eq!(ev2["ts"], "T2");
    }

    // A minter whose `mint_control` parks until the test hands it a permit, so a
    // supersede can be forced to land WHILE a token is mid-mint.
    struct GatedMinter {
        gate: Arc<tokio::sync::Semaphore>,
    }

    #[async_trait::async_trait]
    impl ControlTokenMinter for GatedMinter {
        async fn mint_control(&self, server: &str) -> Result<MintedControlToken, String> {
            self.gate.acquire().await.unwrap().forget();
            Ok(MintedControlToken {
                token: format!("tok-{server}"),
                expires_at: None,
            })
        }
    }

    fn big_detail() -> String {
        // Larger than any socket send buffer, so a single `write_all` to a
        // non-reading peer blocks and (once queued) trips the 5s write deadline.
        "x".repeat(4 * 1024 * 1024)
    }

    /// Bug 1: a stuck reader (connected, never reads) must fail an in-flight approval
    /// on the ~5s write deadline, NOT after the full (here 60s) approval timeout.
    #[tokio::test]
    async fn stuck_reader_fails_approval_on_write_deadline() {
        let h = Harness::start(None, vec![], Duration::from_secs(60));
        let mut app = TestApp::connect(&h.path).await;
        app.handshake(0).await; // reads its hello_ack, then stops reading
        let ready = h.server.clone();
        assert!(wait_until(move || ready.consumer_info().is_some()).await);

        // Block the writer task on one oversized frame the stuck reader never drains.
        h.server.publish_audit(&AuditEntryView {
            ts: "T".into(),
            ns: "ssh-agent".into(),
            op: "sign".into(),
            result: "ok".into(),
            detail: big_detail(),
            ..Default::default()
        });

        let server = h.server.clone();
        let started = std::time::Instant::now();
        let outcome = server
            .request_approval("ssh-agent", "sign", "", "web", "d")
            .await;
        let elapsed = started.elapsed();
        assert!(
            !outcome.approved,
            "stuck writer must fail the approval closed"
        );
        assert!(
            elapsed < Duration::from_secs(20),
            "approval must fail on the write deadline (~5s), not the full timeout; took {elapsed:?}"
        );
        // The dead transport was demoted.
        let gone = h.server.clone();
        assert!(wait_until(move || gone.consumer_info().is_none()).await);
    }

    /// Bug 2: a `token.get` in flight when the connection is superseded must NOT
    /// deliver the minted control token to the superseded connection; a later active
    /// consumer still works.
    #[tokio::test]
    async fn superseded_token_get_not_delivered_to_old_connection() {
        let gate = Arc::new(tokio::sync::Semaphore::new(0));
        let minter = Arc::new(GatedMinter { gate: gate.clone() });
        let h = Harness::start(Some(minter), vec![], Duration::from_secs(25));

        let mut a1 = TestApp::connect(&h.path).await;
        a1.handshake(0).await;
        let ready = h.server.clone();
        assert!(wait_until(move || ready.consumer_info().is_some()).await);
        // a1 requests a token; the minter parks (0 permits) so the token task is
        // stuck mid-mint holding a reference to a1's connection id.
        a1.send(json!({"type": "token.get", "id": "q1", "server": "mini2"}))
            .await;

        // a2 supersedes a1.
        let mut a2 = TestApp::connect(&h.path).await;
        a2.handshake(0).await;
        // a1 receives ONLY the superseded ack (its receipt proves the supersede
        // completed before we release the mint).
        let ack = a1.recv().await;
        assert_eq!(ack["type"], "hello_ack");
        assert_eq!(ack["accepted"], false);
        assert_eq!(ack["reason"], "superseded");

        // Release a1's mint: it completes AFTER the supersede, sees a2 is now the
        // active consumer, and drops the token.
        gate.add_permits(1);
        // a1 must receive nothing further — its connection is closed (EOF).
        let mut line = Vec::new();
        let n = a1.reader.read_until(b'\n', &mut line).await.unwrap();
        assert_eq!(
            n, 0,
            "a superseded connection must never receive a token.response"
        );

        // a2 (the active consumer) still mints successfully.
        a2.send(json!({"type": "token.get", "id": "q2", "server": "mini3"}))
            .await;
        gate.add_permits(1);
        let resp = a2.recv().await;
        assert_eq!(resp["type"], "token.response");
        assert_eq!(resp["in_reply_to"], "q2");
        assert_eq!(resp["server"], "mini3");
        assert_eq!(resp["token"], "tok-mini3");
    }

    /// Bug 3: overflowing the bounded writer queue (a stalled reader) is a transport
    /// failure — it demotes the consumer and fail-closes an in-flight approval.
    #[tokio::test]
    async fn queue_overflow_demotes_and_fails_pending() {
        let h = Harness::start(None, vec![], Duration::from_secs(60));
        let mut app = TestApp::connect(&h.path).await;
        app.handshake(0).await; // then stops reading
        let ready = h.server.clone();
        assert!(wait_until(move || ready.consumer_info().is_some()).await);

        // Block the writer so nothing drains and the queue can actually fill.
        h.server.publish_audit(&AuditEntryView {
            ts: "T".into(),
            ns: "ssh-agent".into(),
            op: "sign".into(),
            result: "ok".into(),
            detail: big_detail(),
            ..Default::default()
        });

        // Register an in-flight approval (pending + one queued frame).
        let server = h.server.clone();
        let req = tokio::spawn(async move {
            server
                .request_approval("ssh-agent", "sign", "", "web", "d")
                .await
        });
        tokio::time::sleep(Duration::from_millis(50)).await;

        // Flood past the bounded capacity -> overflow -> demote + fail-close. The
        // flood is synchronous, so overflow demotes within ~ms.
        for _ in 0..(WRITER_QUEUE_CAP + 50) {
            h.server.publish_audit(&AuditEntryView {
                ts: "t".into(),
                ns: "ssh-agent".into(),
                op: "sign".into(),
                result: "ok".into(),
                ..Default::default()
            });
        }

        // The overflow path must fail-close well before the 5s write-deadline
        // fallback (which would also demote) — a 3s bound proves it was the overflow,
        // not the deadline. (Pre-fix, `publish_audit` ignored overflow, so only the
        // 5s deadline path resolved this, blowing the 3s bound.)
        let outcome = tokio::time::timeout(Duration::from_secs(3), req)
            .await
            .expect("overflow must fail the approval before the 5s write deadline")
            .unwrap();
        assert!(!outcome.approved);
        let gone = h.server.clone();
        assert!(wait_until(move || gone.consumer_info().is_none()).await);
    }

    /// Bug 4: a frame one byte over `MAX_FRAME_BYTES` that ends in a newline must
    /// disconnect (the cap is now re-checked in the newline-found branch), never be
    /// accepted just because it happened to terminate.
    #[tokio::test]
    async fn over_cap_newline_frame_disconnects() {
        let h = Harness::start(None, vec![], Duration::from_secs(25));
        let mut app = TestApp::connect(&h.path).await;
        app.handshake(0).await;
        let ready = h.server.clone();
        assert!(wait_until(move || ready.consumer_info().is_some()).await);

        // Exactly MAX_FRAME_BYTES data bytes + '\n' -> buf hits MAX_FRAME_BYTES+1 the
        // moment the newline is consumed. Pre-fix this returned Ok(true) (accepted);
        // now it errors and the server drops the connection.
        let mut over = vec![b'x'; MAX_FRAME_BYTES];
        over.push(b'\n');
        app.write_half.write_all(&over).await.unwrap();

        let gone = h.server.clone();
        assert!(
            wait_until(move || gone.consumer_info().is_none()).await,
            "an over-cap newline-terminated frame must disconnect the consumer"
        );
    }
}
