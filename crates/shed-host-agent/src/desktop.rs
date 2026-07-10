//! The desktop approval channel — the stateful async UDS server, ported from the
//! Go daemon's `desktop_server.go` / `desktop_gate.go` (catalog §8). It exposes
//! the all-namespace audit/event stream plus the approval request/response and
//! `token.get`/`token.response` channels to a single active consumer (normally the
//! shed-desktop app). Wire shapes + framing/correlation come from
//! `shed_core::approval::protocol` (the shared codec, server direction).
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

use shed_core::approval::protocol::{
    self, ApprovalResponseMsg, AuditEntryView, ClientInfo, DesktopInbound, TokenGetMsg,
};

use crate::config::{NS_AWS_CREDENTIALS, NS_DOCKER_CREDENTIALS, NS_SSH_AGENT};
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

/// A stand-in minter that returns a canned token — lets the daemon answer
/// `token.get` end-to-end before the real minter lands. NOT for production use.
pub struct StubControlMinter;

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

/// The result of a delegated approval, mirroring the Go `desktopGate.Approve`
/// outcome. On a received decision (approve OR deny), `decided_by` defaults to
/// `"user"` when the app left it empty; on a no-decision fail-closed (no consumer,
/// timeout, disconnect, transport error) `decided_by` is empty (matching Go's
/// empty `ApprovalOutcome{}` on the error path).
//
// `request_approval` (its only producer) has no bus/backend caller until slice
// 1b/1c, so the outcome type + its mapping helpers read as dead until then.
#[allow(dead_code)]
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ApprovalOutcome {
    pub approved: bool,
    pub decided_by: String,
    pub scope: Option<String>,
    pub ttl: Option<String>,
}

/// The app's decision as delivered internally (raw, before the `decided_by`
/// default is applied). Mirrors Go's `desktopDecision`. The fields are read by
/// `decision_to_outcome` (caller-less until slice 1b/1c).
#[allow(dead_code)]
struct DesktopDecision {
    approved: bool,
    decided_by: String,
    scope: Option<String>,
    ttl: Option<String>,
}

#[allow(dead_code)] // via request_approval, wired in slice 1b/1c
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

/// The fail-closed outcome when NO decision was made (no consumer, timeout,
/// disconnect, transport error): denied with an empty `decided_by`, matching the
/// Go gate's `ApprovalOutcome{}` on the error path.
#[allow(dead_code)] // via request_approval, wired in slice 1b/1c
fn deny_no_decision() -> ApprovalOutcome {
    ApprovalOutcome {
        approved: false,
        decided_by: String::new(),
        scope: None,
        ttl: None,
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
    /// Send onto this to write a frame to the consumer.
    writer: mpsc::UnboundedSender<Vec<u8>>,
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
    /// `DesktopServer.RequestApproval` + `desktopGate.Approve` into one call. No
    /// bus/backend caller until slice 1b/1c.
    #[allow(dead_code)]
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
        let writer = {
            let mut inner = self.inner.lock().unwrap();
            let Some(consumer) = inner.consumer.as_ref() else {
                return deny_no_decision(); // no consumer -> fail closed
            };
            let owner = consumer.id;
            let writer = consumer.writer.clone();
            // Register BEFORE writing so a fast reply can't race ahead of registration.
            inner.pending.insert(id.clone(), Pending { tx, owner });
            writer
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
        if writer.send(with_newline(frame)).is_err() {
            self.inner.lock().unwrap().pending.remove(&id);
            return deny_no_decision(); // transport gone
        }

        let outcome = tokio::select! {
            res = rx => match res {
                Ok(dec) => decision_to_outcome(dec),
                Err(_) => deny_no_decision(), // sender dropped without a decision
            },
            _ = tokio::time::sleep(self.timeout) => deny_no_decision(),
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
        let consumer_writer = {
            let mut inner = self.inner.lock().unwrap();
            inner.ring.push(frame.clone());
            let overflow = inner.ring.len().saturating_sub(RING_MAX);
            if overflow > 0 {
                inner.ring.drain(0..overflow);
            }
            inner.consumer.as_ref().map(|c| c.writer.clone())
        };
        if let Some(writer) = consumer_writer {
            let _ = writer.send(with_newline(frame));
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

        // The writer task owns the write half from here (5s per-frame deadline).
        let (writer_tx, writer_rx) = mpsc::unbounded_channel::<Vec<u8>>();
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
                    // must not stall this read loop (and thus approvals).
                    let this = Arc::clone(&self);
                    let writer = writer_tx.clone();
                    tokio::spawn(async move { this.handle_token_get(&writer, req).await });
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
        if tokio::time::timeout(
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
    async fn handle_token_get(&self, writer: &mpsc::UnboundedSender<Vec<u8>>, req: TokenGetMsg) {
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
        let _ = writer.send(with_newline(frame));
    }

    fn promote(
        &self,
        conn_id: u64,
        client: ClientInfo,
        writer: mpsc::UnboundedSender<Vec<u8>>,
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
                let _ = old_writer.send(with_newline(ack));
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

    fn replay(&self, writer: &mpsc::UnboundedSender<Vec<u8>>, n: i64) {
        if n <= 0 {
            return;
        }
        let frames = {
            let inner = self.inner.lock().unwrap();
            let start = inner.ring.len().saturating_sub(n as usize);
            inner.ring[start..].to_vec()
        };
        for frame in frames {
            let _ = writer.send(with_newline(frame));
        }
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

async fn writer_loop(mut write_half: OwnedWriteHalf, mut rx: mpsc::UnboundedReceiver<Vec<u8>>) {
    while let Some(bytes) = rx.recv().await {
        match tokio::time::timeout(CONSUMER_WRITE_TIMEOUT, write_half.write_all(&bytes)).await {
            Ok(Ok(())) => {}
            // A write past the deadline or an io error drops the connection (fail
            // closed): dropping `rx` here closes the channel so later sends fail fast.
            _ => return,
        }
    }
}

async fn ping_loop(writer: mpsc::UnboundedSender<Vec<u8>>) {
    let mut ticker = tokio::time::interval(PING_INTERVAL);
    // tokio's interval fires immediately on the first tick; Go's ticker fires only
    // after the period, so consume the first tick to match (first ping at +10s).
    ticker.tick().await;
    loop {
        ticker.tick().await;
        let frame = protocol::ping(&new_id(), &now_rfc3339());
        if writer.send(with_newline(frame)).is_err() {
            return; // consumer gone
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
        if found {
            return Ok(true);
        }
        if buf.len() > max {
            return Err(std::io::Error::new(
                std::io::ErrorKind::InvalidData,
                "desktop frame exceeded the size cap",
            ));
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
}
