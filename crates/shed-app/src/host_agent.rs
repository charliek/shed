//! The shed-host-agent UDS client — the stateful state machine ported from
//! `HostAgentClient.swift`. Connects to the agent's socket, registers with a
//! `hello`, streams inbound frames (approval requests + the all-namespace audit
//! feed), answers pings, sends approve/deny responses, and correlates
//! `token.get`/`token.response` for control-token minting. Auto-reconnects with
//! backoff.
//!
//! **Fail-closed:** when not connected, `respond` is a no-op (the agent denies
//! on its side, which is correct — F2) and in-flight `token.get` requests fail
//! (F10). `respond` is a synchronous, non-blocking send onto the writer channel
//! (never awaited under the state lock), so the coordinator can call it inside
//! its atomic critical section (§2.2). Single-resume of a correlated request is
//! structural: a `oneshot::Sender` is consumed on send and `HashMap::remove`
//! hands it to exactly one path (reply | timeout | disconnect).

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::unix::{OwnedReadHalf, OwnedWriteHalf};
use tokio::net::UnixStream;
use tokio::sync::{mpsc, oneshot};
use tokio::task::JoinHandle;

use shed_core::approval::protocol::{self, HostAgentInbound};
use shed_core::approval::{
    ApprovalDecision, CredentialResponse, DecidedBy, HelloAck, TokenResponse,
};

use crate::traits::ClockRef;

const INITIAL_BACKOFF: Duration = Duration::from_millis(500);
const MAX_BACKOFF: Duration = Duration::from_secs(5);
/// Default per-request timeout for `token.get` (mirrors the Swift 10s).
pub const DEFAULT_TOKEN_TIMEOUT: Duration = Duration::from_secs(10);
/// Max bytes per newline-framed message (mirrors the mac `ipcMaxFrameBytes`); a
/// larger frame is a protocol violation → disconnect, never unbounded growth.
const MAX_FRAME_BYTES: usize = 1 << 20; // 1 MiB

/// The client's registration payload (`hello`).
#[derive(Debug, Clone)]
pub struct HelloClientInfo {
    pub name: String,
    pub version: String,
    pub pid: i32,
    pub capabilities: Vec<String>,
    pub replay_events: i64,
}

/// Connection + frame events emitted to the coordinator. `Frame` is boxed — the
/// inbound frame (an approval request / audit event) is much larger than the
/// other variants.
#[derive(Debug)]
pub enum HostAgentEvent {
    Connected(HelloAck),
    Disconnected,
    /// The socket peer failed the A1 peer-UID check (server not running as us) —
    /// a distinct, audited state so the UI isn't left silently loop-with-gate-down.
    Untrusted,
    Frame(Box<HostAgentInbound>),
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum HostAgentClientError {
    NotConnected,
    TimedOut,
    Disconnected,
    /// The connected agent does not advertise a capability this request needs.
    ///
    /// It is a distinct variant, and it is checked BEFORE the frame is written,
    /// because the alternative is indistinguishable from a hang: an agent that
    /// predates a message drops it silently, so the app would sit through the
    /// full timeout and then report "timed out" for what is really a version
    /// mismatch with a one-line fix.
    Unsupported(&'static str),
    /// The capability the caller decided on is no longer the one this connection
    /// holds — the connection was replaced (or its ack withdrawn) between the
    /// decision and the send. Distinct from `Unsupported` because the fix is
    /// "try again in a moment", not "upgrade something".
    CapabilityLost,
    /// The CSR handed down is larger than this socket is willing to carry — the
    /// outbound half of the credential size caps. Never produced by the core's
    /// own keypairs; a guard against relaying something that is not a CSR.
    OversizedCsr(usize),
}

impl std::fmt::Display for HostAgentClientError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        let s = match self {
            HostAgentClientError::NotConnected => "host agent not connected",
            HostAgentClientError::TimedOut => "timed out waiting for host agent reply",
            HostAgentClientError::Disconnected => "host agent connection dropped",
            HostAgentClientError::Unsupported(cap) => {
                return write!(
                f,
                "the connected shed-host-agent does not support `{cap}`; upgrade shed-host-agent"
            )
            }
            HostAgentClientError::CapabilityLost => {
                "the shed-host-agent connection changed before the request could be sent"
            }
            HostAgentClientError::OversizedCsr(bytes) => {
                return write!(
                    f,
                    "refusing to send a {bytes}-byte CSR (cap {})",
                    crate::token_minter::limits::MAX_CSR_BYTES
                )
            }
        };
        f.write_str(s)
    }
}

impl std::error::Error for HostAgentClientError {}

struct State {
    /// `Some` only while connected — the write channel to the current
    /// connection's writer task. Its absence is the fail-closed signal.
    writer: Option<mpsc::UnboundedSender<Vec<u8>>>,
    /// In-flight `token.get` requests keyed by request id, each awaiting the
    /// correlated `token.response` (matched by `in_reply_to`). `remove` is the
    /// single-resume guard — whoever removes the sender owns its resume.
    pending: HashMap<String, oneshot::Sender<TokenResponse>>,
    /// The same, for `credential.get`. A separate map rather than one keyed on a
    /// sum type: the two replies are different frames with different fields, and
    /// a shared map would have to carry a runtime discriminant whose only job
    /// would be to turn a mismatch into a panic or a silent drop.
    pending_credentials: HashMap<String, oneshot::Sender<CredentialResponse>>,
    /// What the connected agent advertised in its ACCEPTED `hello_ack`.
    ///
    /// `None` is the tri-state's `Unknown`: no accepted ack has been seen on the
    /// CURRENT connection (startup, reconnect, a rejected/superseded ack).
    /// `Some(list)` — possibly empty — is a real answer from a live agent, and
    /// an empty list is what an agent too old to advertise anything sends.
    /// Conflating the two is the §7 P5 bug: "we have not asked yet" would become
    /// "the agent is old", producing either a false upgrade error or a silent
    /// `token.get` against a certificate-only server.
    capabilities: Option<Vec<String>>,
    /// Bumped on every connection change (connect, disconnect, superseded ack),
    /// so a capability answer can be BOUND to the connection it was learned on
    /// and re-checked at send time.
    generation: u64,
}

/// What this connection knows about the agent's `credential.get` support
/// (plan 002 §7 P5).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AgentCapabilityState {
    /// No accepted `hello_ack` seen yet on the current connection.
    Unknown,
    /// The ack advertised `credential.get`.
    Supported,
    /// The ack arrived WITHOUT `credential.get` — a shipped older agent.
    Unsupported,
}

/// A capability answer plus the connection generation it was learned on. Passing
/// it back into [`HostAgentClient::request_credential`] is what makes the
/// decision and the send atomic with respect to a reconnect.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct CapabilitySnapshot {
    pub state: AgentCapabilityState,
    pub generation: u64,
}

struct Inner {
    socket_path: PathBuf,
    clock: ClockRef,
    running: AtomicBool,
    state: Mutex<State>,
    loop_handle: Mutex<Option<JoinHandle<()>>>,
    /// Publishes every capability transition so a pre-ack caller can AWAIT the
    /// answer instead of guessing. A watch (not a Notify) so a waiter that
    /// subscribes after the ack still sees the current value immediately.
    cap_tx: tokio::sync::watch::Sender<CapabilitySnapshot>,
}

impl Inner {
    /// Replace the current connection's capability answer and publish it.
    /// `caps` is `None` for "unknown" (connect/disconnect/rejected ack).
    /// The generation always advances, so any snapshot taken before this call is
    /// now stale by construction.
    /// Learn this connection's capabilities from a `hello_ack`.
    ///
    /// Only an ACCEPTED ack teaches anything: a rejection means the agent
    /// declined this client, so whatever it listed describes a session we do not
    /// have. A second ack on the same connection supersedes the first (and bumps
    /// the generation), so a decision made against the earlier one cannot be
    /// spent.
    fn apply_hello_ack(&self, ack: &HelloAck) {
        self.set_capabilities(ack.accepted.then(|| ack.agent_capabilities.clone()));
    }

    fn set_capabilities(&self, caps: Option<Vec<String>>) {
        let snapshot = {
            let mut st = self.state.lock().unwrap();
            st.capabilities = caps;
            st.generation = st.generation.wrapping_add(1);
            capability_snapshot(&st)
        };
        // Ignore a send error: it only means nothing is currently awaiting.
        let _ = self.cap_tx.send(snapshot);
    }
}

/// The tri-state + generation for a locked state. Free fn so both the public
/// readers and the send-time re-check share one derivation.
fn capability_snapshot(st: &State) -> CapabilitySnapshot {
    let state = match &st.capabilities {
        None => AgentCapabilityState::Unknown,
        Some(caps) if caps.iter().any(|c| c == protocol::CAP_CREDENTIAL_GET) => {
            AgentCapabilityState::Supported
        }
        Some(_) => AgentCapabilityState::Unsupported,
    };
    CapabilitySnapshot {
        state,
        generation: st.generation,
    }
}

/// A shareable handle to the host-agent connection. Cloneable so the coordinator
/// (which `respond`s + consumes events) and the token minter (which
/// `request_token`s) can both hold it.
#[derive(Clone)]
pub struct HostAgentClient {
    inner: Arc<Inner>,
}

impl HostAgentClient {
    pub fn new(socket_path: impl Into<PathBuf>, clock: ClockRef) -> Self {
        let (cap_tx, _) = tokio::sync::watch::channel(CapabilitySnapshot {
            state: AgentCapabilityState::Unknown,
            generation: 0,
        });
        Self {
            inner: Arc::new(Inner {
                socket_path: socket_path.into(),
                clock,
                running: AtomicBool::new(false),
                state: Mutex::new(State {
                    writer: None,
                    pending: HashMap::new(),
                    pending_credentials: HashMap::new(),
                    capabilities: None,
                    generation: 0,
                }),
                loop_handle: Mutex::new(None),
                cap_tx,
            }),
        }
    }

    /// Start connecting and return a stream of connection + frame events. The
    /// background loop runs until `stop()` (or the process exits).
    pub fn start(&self, info: HelloClientInfo) -> mpsc::UnboundedReceiver<HostAgentEvent> {
        // Restart-safe: abort any prior loop + reset connection state so a second
        // start() cleanly replaces the first rather than orphaning it.
        if let Some(h) = self.inner.loop_handle.lock().unwrap().take() {
            h.abort();
        }
        {
            let mut st = self.inner.state.lock().unwrap();
            st.writer = None;
            st.pending.clear();
            st.pending_credentials.clear();
        }
        self.inner.set_capabilities(None);
        let (event_tx, event_rx) = mpsc::unbounded_channel();
        self.inner.running.store(true, Ordering::SeqCst);
        let inner = self.inner.clone();
        let handle = tokio::spawn(async move { run_loop(inner, info, event_tx).await });
        *self.inner.loop_handle.lock().unwrap() = Some(handle);
        event_rx
    }

    /// Stop the loop and fail any in-flight requests (fail-closed).
    pub fn stop(&self) {
        self.inner.running.store(false, Ordering::SeqCst);
        if let Some(h) = self.inner.loop_handle.lock().unwrap().take() {
            h.abort();
        }
        {
            let mut st = self.inner.state.lock().unwrap();
            st.writer = None;
            // Dropping the senders fails everything awaiting a reply.
            st.pending.clear();
            st.pending_credentials.clear();
        }
        self.inner.set_capabilities(None);
    }

    pub fn is_connected(&self) -> bool {
        self.inner.state.lock().unwrap().writer.is_some()
    }

    /// Send an approve/deny for a request. A no-op (→ the agent fails closed) if
    /// not currently connected. Synchronous + non-blocking.
    pub fn respond(
        &self,
        request_id: &str,
        decision: ApprovalDecision,
        decided_by: DecidedBy,
        scope: Option<&str>,
        ttl: Option<&str>,
    ) {
        let line = protocol::approval_response(
            &new_id(),
            &self.inner.clock.now_iso8601(),
            request_id,
            decision,
            decided_by,
            scope,
            ttl,
        );
        self.write_line(line);
    }

    /// Request a CONTROL token for `server`. Sends a `token.get` and awaits the
    /// correlated `token.response`. `Err(NotConnected)` if there is no live
    /// connection, `Err(TimedOut)` if no reply arrives within `timeout`,
    /// `Err(Disconnected)` if the connection drops while waiting. A fail-closed
    /// reply (its `error` set, `token` `None`) is returned in the `TokenResponse`
    /// — the caller inspects it; it is not an `Err`.
    pub async fn request_token(
        &self,
        server: &str,
        timeout: Duration,
    ) -> Result<TokenResponse, HostAgentClientError> {
        let id = new_id();
        let (tx, rx) = oneshot::channel();
        {
            // Register BEFORE writing so a fast reply can't race ahead of
            // registration. The write is a non-blocking channel send, so holding
            // the state lock across it is fine.
            let mut st = self.inner.state.lock().unwrap();
            let Some(writer) = st.writer.clone() else {
                return Err(HostAgentClientError::NotConnected);
            };
            st.pending.insert(id.clone(), tx);
            if writer
                .send(with_newline(protocol::token_get(&id, server)))
                .is_err()
            {
                st.pending.remove(&id);
                return Err(HostAgentClientError::NotConnected);
            }
        }
        match tokio::time::timeout(timeout, rx).await {
            Ok(Ok(resp)) => Ok(resp),
            // The sender was dropped (disconnect/stop failed all pending).
            Ok(Err(_)) => Err(HostAgentClientError::Disconnected),
            Err(_) => {
                // Timed out — drop our sender so a late reply is a no-op.
                self.inner.state.lock().unwrap().pending.remove(&id);
                Err(HostAgentClientError::TimedOut)
            }
        }
    }

    /// Whether the connected agent DEFINITELY advertises `capability`. `false`
    /// while disconnected or pre-ack — callers that must distinguish "no" from
    /// "not known yet" use [`Self::credential_capability`] instead.
    pub fn supports(&self, capability: &str) -> bool {
        self.inner
            .state
            .lock()
            .unwrap()
            .capabilities
            .as_ref()
            .is_some_and(|caps| caps.iter().any(|c| c == capability))
    }

    /// The tri-state `credential.get` capability of the CURRENT connection.
    /// A cached read — never I/O, so it is safe under the provider's mint mutex.
    pub fn credential_capability(&self) -> AgentCapabilityState {
        self.credential_capability_snapshot().state
    }

    /// As [`Self::credential_capability`], carrying the connection generation so
    /// the decision can be re-checked when the frame is finally written.
    pub fn credential_capability_snapshot(&self) -> CapabilitySnapshot {
        capability_snapshot(&self.inner.state.lock().unwrap())
    }

    /// Wait (bounded) for this connection to learn its capability, returning the
    /// first non-`Unknown` snapshot or the still-unknown one on timeout.
    ///
    /// Only an mtls-expecting mint should pay this: a token-mode server learns
    /// nothing useful from the ack and keeps every shipped build's immediate
    /// `token.get`.
    pub async fn await_credential_capability(&self, timeout: Duration) -> CapabilitySnapshot {
        let mut rx = self.inner.cap_tx.subscribe();
        // Re-read under the lock first: the ack may have landed between the
        // caller's snapshot and the subscribe.
        let current = self.credential_capability_snapshot();
        if current.state != AgentCapabilityState::Unknown {
            return current;
        }
        let wait = async {
            loop {
                if rx.changed().await.is_err() {
                    // The sender lives in `Inner`, which this handle keeps
                    // alive; unreachable in practice, but never spin on it.
                    return self.credential_capability_snapshot();
                }
                let snapshot = *rx.borrow_and_update();
                if snapshot.state != AgentCapabilityState::Unknown {
                    return snapshot;
                }
            }
        };
        tokio::time::timeout(timeout, wait)
            .await
            .unwrap_or_else(|_| self.credential_capability_snapshot())
    }

    /// Request a CONTROL credential for `server` in whichever shape that server
    /// issues, relaying `csr_base64` — a CSR THIS process generated, whose
    /// private key never leaves it.
    ///
    /// The capability is checked first, so an agent too old to know the message
    /// produces `Unsupported` immediately rather than a timeout: an old agent
    /// does not reject an unknown frame, it drops it, and "no reply ever" is
    /// indistinguishable from a hang without this check.
    ///
    /// As with `request_token`, a fail-closed reply (its `error` set) comes back
    /// inside the `CredentialResponse` for the caller to inspect; it is not an
    /// `Err`.
    pub async fn request_credential(
        &self,
        server: &str,
        csr_base64: Option<&str>,
        capability: CapabilitySnapshot,
        timeout: Duration,
    ) -> Result<CredentialResponse, HostAgentClientError> {
        if capability.state != AgentCapabilityState::Supported {
            return Err(HostAgentClientError::Unsupported(
                protocol::CAP_CREDENTIAL_GET,
            ));
        }
        // The outbound half of the size caps: we refuse to PUT an oversized
        // value on this socket, not only to accept one from it.
        if let Some(csr) = csr_base64 {
            if csr.len() > crate::token_minter::limits::MAX_CSR_BYTES {
                return Err(HostAgentClientError::OversizedCsr(csr.len()));
            }
        }
        let id = new_id();
        let (tx, rx) = oneshot::channel();
        {
            let mut st = self.inner.state.lock().unwrap();
            // Re-check the capability against the connection we are about to
            // write to, under the SAME lock that hands out the writer. Between
            // the caller's decision and here the socket may have reconnected —
            // and the new agent on the other end need not be the old one.
            if capability_snapshot(&st) != capability {
                return Err(HostAgentClientError::CapabilityLost);
            }
            let Some(writer) = st.writer.clone() else {
                return Err(HostAgentClientError::NotConnected);
            };
            st.pending_credentials.insert(id.clone(), tx);
            if writer
                .send(with_newline(protocol::credential_get(
                    &id, server, csr_base64,
                )))
                .is_err()
            {
                st.pending_credentials.remove(&id);
                return Err(HostAgentClientError::NotConnected);
            }
        }
        match tokio::time::timeout(timeout, rx).await {
            Ok(Ok(resp)) => Ok(resp),
            Ok(Err(_)) => Err(HostAgentClientError::Disconnected),
            Err(_) => {
                self.inner
                    .state
                    .lock()
                    .unwrap()
                    .pending_credentials
                    .remove(&id);
                Err(HostAgentClientError::TimedOut)
            }
        }
    }

    fn write_line(&self, line: String) {
        let st = self.inner.state.lock().unwrap();
        if let Some(writer) = &st.writer {
            let _ = writer.send(with_newline(line));
        }
    }
}

impl crate::traits::Responder for HostAgentClient {
    fn respond(
        &self,
        request_id: &str,
        decision: ApprovalDecision,
        decided_by: DecidedBy,
        scope: Option<&str>,
        ttl: Option<&str>,
    ) {
        HostAgentClient::respond(self, request_id, decision, decided_by, scope, ttl);
    }
}

fn new_id() -> String {
    uuid::Uuid::new_v4().to_string()
}

fn with_newline(line: String) -> Vec<u8> {
    let mut b = line.into_bytes();
    b.push(b'\n');
    b
}

async fn run_loop(
    inner: Arc<Inner>,
    info: HelloClientInfo,
    event_tx: mpsc::UnboundedSender<HostAgentEvent>,
) {
    let mut backoff = INITIAL_BACKOFF;
    while inner.running.load(Ordering::SeqCst) {
        // F11: reject a symlink/non-socket at the path before connecting, then
        // connect. Either failing → back off + retry (a legit agent eventually
        // places a real socket).
        let connected = if socket_is_trustworthy(&inner.socket_path) {
            UnixStream::connect(&inner.socket_path).await.ok()
        } else {
            None
        };
        let Some(stream) = connected else {
            tokio::time::sleep(backoff).await;
            backoff = (backoff * 2).min(MAX_BACKOFF);
            continue;
        };
        // A1: the host agent must run as us. Reject + back off BEFORE trusting the
        // stream (before `into_split` / setting the writer), so we never send a
        // frame to a wrong-UID peer. A same-UID squatter still passes — that
        // residual is covered by the `$XDG_RUNTIME_DIR` 0700 dir + the F11 check;
        // this closes the weak-perms cases (the `/tmp` fallback, a mis-permissioned
        // dir, and macOS where the socket resolves under `~/.local/share`).
        if !peer_trusted(peer_uid(&stream), our_uid()) {
            let _ = event_tx.send(HostAgentEvent::Untrusted);
            tokio::time::sleep(backoff).await;
            backoff = (backoff * 2).min(MAX_BACKOFF);
            continue;
        }
        backoff = INITIAL_BACKOFF;
        let (read_half, write_half) = stream.into_split();
        let (writer_tx, writer_rx) = mpsc::unbounded_channel::<Vec<u8>>();
        let mut writer_task = tokio::spawn(writer_loop(write_half, writer_rx));
        inner.state.lock().unwrap().writer = Some(writer_tx.clone());
        // A NEW connection knows nothing yet, and its generation must differ
        // from the previous one's so an in-flight decision cannot be spent here.
        inner.set_capabilities(None);

        // Register with a hello.
        let hello = protocol::hello(
            &new_id(),
            &inner.clock.now_iso8601(),
            &info.name,
            &info.version,
            info.pid,
            &info.capabilities,
            info.replay_events,
        );
        let _ = writer_tx.send(with_newline(hello));

        // Either the reader ending (EOF/error/over-cap) OR the writer task dying
        // (a write error while the read side stays silent) is a disconnect — so
        // an in-flight token.get fails fast (F10) rather than waiting for its
        // per-request timeout.
        tokio::select! {
            _ = read_frames(&inner, read_half, &writer_tx, &event_tx) => {}
            _ = &mut writer_task => {}
        }

        // Disconnected: clear the writer + fail any in-flight token requests so
        // awaiting callers don't hang until their individual timeout fires.
        {
            let mut st = inner.state.lock().unwrap();
            st.writer = None;
            st.pending.clear();
            st.pending_credentials.clear();
        }
        inner.set_capabilities(None);
        writer_task.abort();
        let _ = event_tx.send(HostAgentEvent::Disconnected);
        if !inner.running.load(Ordering::SeqCst) {
            break;
        }
        tokio::time::sleep(INITIAL_BACKOFF).await;
    }
}

async fn writer_loop(mut write_half: OwnedWriteHalf, mut rx: mpsc::UnboundedReceiver<Vec<u8>>) {
    while let Some(bytes) = rx.recv().await {
        if write_half.write_all(&bytes).await.is_err() {
            return;
        }
    }
}

async fn read_frames(
    inner: &Arc<Inner>,
    read_half: OwnedReadHalf,
    writer_tx: &mpsc::UnboundedSender<Vec<u8>>,
    event_tx: &mpsc::UnboundedSender<HostAgentEvent>,
) {
    let mut reader = BufReader::new(read_half);
    let mut line = Vec::new();
    loop {
        line.clear();
        match read_frame_capped(&mut reader, &mut line, MAX_FRAME_BYTES).await {
            Ok(true) => {}
            // EOF, a partial frame at EOF (dropped — never processed), or an
            // over-cap frame all mean disconnect.
            Ok(false) | Err(_) => return,
        }
        let trimmed = strip_trailing_newline(&line);
        if trimmed.is_empty() {
            continue;
        }
        let frame = match protocol::decode(trimmed) {
            Ok(f) => f,
            Err(_) => continue, // skip a malformed line
        };
        match frame {
            HostAgentInbound::Ping { id } => {
                let pong = protocol::pong(&id, &inner.clock.now_iso8601());
                let _ = writer_tx.send(with_newline(pong));
            }
            HostAgentInbound::HelloAck(ack) => {
                inner.apply_hello_ack(&ack);
                let _ = event_tx.send(HostAgentEvent::Connected(ack));
            }
            HostAgentInbound::TokenResponse(resp) => resolve_pending(inner, resp),
            HostAgentInbound::CredentialResponse(resp) => resolve_pending_credential(inner, resp),
            other => {
                let _ = event_tx.send(HostAgentEvent::Frame(Box::new(other)));
            }
        }
    }
}

/// Resume the request matching `resp.in_reply_to`. A no-op if it already timed
/// out or was failed by a disconnect (`remove` is the single-resume guard).
fn resolve_pending(inner: &Arc<Inner>, resp: TokenResponse) {
    let tx = inner
        .state
        .lock()
        .unwrap()
        .pending
        .remove(&resp.in_reply_to);
    if let Some(tx) = tx {
        let _ = tx.send(resp); // oneshot: consumed on send; a dropped rx is fine
    }
}

/// Resume the `credential.get` matching `resp.in_reply_to`. Same single-resume
/// guard as `resolve_pending`.
fn resolve_pending_credential(inner: &Arc<Inner>, resp: CredentialResponse) {
    let tx = inner
        .state
        .lock()
        .unwrap()
        .pending_credentials
        .remove(&resp.in_reply_to);
    if let Some(tx) = tx {
        let _ = tx.send(resp);
    }
}

fn strip_trailing_newline(line: &[u8]) -> &[u8] {
    let mut end = line.len();
    while end > 0 && (line[end - 1] == b'\n' || line[end - 1] == b'\r') {
        end -= 1;
    }
    &line[..end]
}

/// F11: reject a symlink or non-socket at the well-known path before connecting
/// (defends against socket-squatting in a shared runtime dir). `symlink_metadata`
/// does NOT follow the link, so a squatter's symlink reports as a symlink, not a
/// socket. Peer-UID validation (A1) then checks who's on the other end.
fn socket_is_trustworthy(path: &std::path::Path) -> bool {
    use std::os::unix::fs::FileTypeExt;
    std::fs::symlink_metadata(path)
        .map(|m| m.file_type().is_socket())
        .unwrap_or(false)
}

/// Our own effective UID — the identity the host agent must share.
fn our_uid() -> u32 {
    // SAFETY: getuid is always safe (no args, no failure mode).
    unsafe { libc::getuid() }
}

/// The peer (server) UID on a connected Unix socket. `None` on error → treated as
/// untrusted (fail closed). Linux uses `SO_PEERCRED` (glibc has no `getpeereid`);
/// macOS/iOS use `getpeereid`. Other targets (mobile Android, etc.) return `None`.
fn peer_uid(stream: &UnixStream) -> Option<u32> {
    use std::os::fd::AsRawFd;
    let fd = stream.as_raw_fd();
    #[cfg(target_os = "linux")]
    {
        let mut cred = libc::ucred {
            pid: 0,
            uid: 0,
            gid: 0,
        };
        let mut len = std::mem::size_of::<libc::ucred>() as libc::socklen_t;
        // SAFETY: `fd` is a valid connected AF_UNIX socket for the call's duration;
        // `cred`/`len` are valid, correctly-sized out slots.
        let rc = unsafe {
            libc::getsockopt(
                fd,
                libc::SOL_SOCKET,
                libc::SO_PEERCRED,
                (&mut cred as *mut libc::ucred).cast::<libc::c_void>(),
                &mut len,
            )
        };
        (rc == 0).then_some(cred.uid)
    }
    #[cfg(any(
        target_os = "macos",
        target_os = "ios",
        target_os = "freebsd",
        target_os = "openbsd",
        target_os = "netbsd",
        target_os = "dragonfly"
    ))]
    {
        let mut uid: libc::uid_t = 0;
        let mut gid: libc::gid_t = 0;
        // SAFETY: `fd` is a valid connected AF_UNIX fd; the out-params are valid slots.
        let rc = unsafe { libc::getpeereid(fd, &mut uid, &mut gid) };
        (rc == 0).then_some(uid)
    }
    // Remaining targets — those without `getpeereid` (bionic/Android and other mobile,
    // Windows) — that never bind the host-agent UDS server, so `peer_uid` is never
    // called here. Returning `None` (untrusted) is the correct fail-closed default, and
    // keeps shed-app compiling for those targets (e.g. `aarch64-linux-android`).
    #[cfg(not(any(
        target_os = "linux",
        target_os = "macos",
        target_os = "ios",
        target_os = "freebsd",
        target_os = "openbsd",
        target_os = "netbsd",
        target_os = "dragonfly"
    )))]
    {
        let _ = fd;
        None
    }
}

/// A1 trust rule: the peer must run as us. A lookup failure (`None`) is untrusted.
fn peer_trusted(peer: Option<u32>, ours: u32) -> bool {
    peer == Some(ours)
}

/// Read one newline-terminated frame into `buf` (including the trailing `\n`),
/// capped at `max` bytes. `Ok(true)` = a complete frame; `Ok(false)` = EOF — any
/// partial bytes are dropped (a frame missing its terminator at EOF is never
/// processed, matching the Swift `LineFrameReader`); `Err` on an I/O error or a
/// frame exceeding `max` (a protocol violation → disconnect, no unbounded growth).
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
                "host agent frame exceeded the size cap",
            ));
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::{json, Value};
    use shed_core::approval::CAP_CREDENTIAL_GET;
    use std::sync::Arc;
    use tokio::sync::Mutex as AsyncMutex;

    struct FixedClock;
    impl crate::traits::Clock for FixedClock {
        fn now_unix(&self) -> i64 {
            1_700_000_000
        }
    }

    #[derive(Clone, Copy, PartialEq)]
    enum TokenMode {
        Ok,
        Error,
        Silent,
    }

    struct Records {
        token_gets: Vec<Value>,
        credential_gets: Vec<Value>,
        responses: Vec<Value>,
        hello_count: usize,
        token_seq: u32,
    }

    /// An in-process UDS agent double: auto-`hello_ack`s, records `token.get` +
    /// `approval_response`, auto-replies `token.get` per `token_mode`, and lets a
    /// test push arbitrary frames or drop the live connection.
    struct TestAgent {
        path: PathBuf,
        records: Arc<Mutex<Records>>,
        token_mode: Arc<Mutex<TokenMode>>,
        /// Whether this double advertises `credential.get`. DEFAULT FALSE, i.e.
        /// the double is an OLD agent unless a test says otherwise — so every
        /// pre-existing assertion keeps describing the pairing it was written for,
        /// and the new capability has to be opted into deliberately.
        advertise_credential_get: Arc<Mutex<bool>>,
        write_half: Arc<AsyncMutex<Option<OwnedWriteHalf>>>,
        _accept: JoinHandle<()>,
    }

    impl TestAgent {
        fn start() -> Self {
            let path = std::env::temp_dir().join(format!("shed-ha-test-{}.sock", new_id()));
            let _ = std::fs::remove_file(&path);
            let listener = tokio::net::UnixListener::bind(&path).unwrap();
            let records = Arc::new(Mutex::new(Records {
                token_gets: Vec::new(),
                credential_gets: Vec::new(),
                responses: Vec::new(),
                hello_count: 0,
                token_seq: 0,
            }));
            let token_mode = Arc::new(Mutex::new(TokenMode::Ok));
            let advertise_credential_get = Arc::new(Mutex::new(false));
            let write_half = Arc::new(AsyncMutex::new(None));
            let (r, m, w, c) = (
                records.clone(),
                token_mode.clone(),
                write_half.clone(),
                advertise_credential_get.clone(),
            );
            let accept = tokio::spawn(async move {
                loop {
                    let Ok((stream, _)) = listener.accept().await else {
                        return;
                    };
                    let (read_half, wh) = stream.into_split();
                    *w.lock().await = Some(wh);
                    serve_conn(read_half, r.clone(), m.clone(), w.clone(), c.clone()).await;
                    *w.lock().await = None;
                }
            });
            TestAgent {
                path,
                records,
                token_mode,
                advertise_credential_get,
                write_half,
                _accept: accept,
            }
        }

        fn client(&self, clock: ClockRef) -> HostAgentClient {
            HostAgentClient::new(self.path.clone(), clock)
        }

        async fn write_frame(&self, obj: Value) {
            if let Some(wh) = self.write_half.lock().await.as_mut() {
                let mut bytes = serde_json::to_vec(&obj).unwrap();
                bytes.push(b'\n');
                let _ = wh.write_all(&bytes).await;
            }
        }

        /// Write raw bytes with NO trailing newline (for the partial-frame test).
        async fn write_raw(&self, bytes: &[u8]) {
            if let Some(wh) = self.write_half.lock().await.as_mut() {
                let _ = wh.write_all(bytes).await;
            }
        }

        async fn drop_conn(&self) {
            *self.write_half.lock().await = None; // dropping the write half closes it
        }

        fn set_token_mode(&self, mode: TokenMode) {
            *self.token_mode.lock().unwrap() = mode;
        }

        /// Make this double a NEW agent (advertises `credential.get`).
        fn advertise_credential_get(&self) {
            *self.advertise_credential_get.lock().unwrap() = true;
        }

        fn credential_gets(&self) -> Vec<Value> {
            self.records.lock().unwrap().credential_gets.clone()
        }

        fn hello_count(&self) -> usize {
            self.records.lock().unwrap().hello_count
        }
        fn token_gets(&self) -> Vec<Value> {
            self.records.lock().unwrap().token_gets.clone()
        }
        fn responses(&self) -> Vec<Value> {
            self.records.lock().unwrap().responses.clone()
        }

        async fn wait_hello(&self, n: usize) -> bool {
            wait_until(|| self.hello_count() >= n).await
        }
        async fn wait_token_gets(&self, n: usize) -> bool {
            wait_until(|| self.token_gets().len() >= n).await
        }
        async fn wait_responses(&self, n: usize) -> bool {
            wait_until(|| self.responses().len() >= n).await
        }
    }

    impl Drop for TestAgent {
        fn drop(&mut self) {
            let _ = std::fs::remove_file(&self.path);
        }
    }

    async fn serve_conn(
        read_half: OwnedReadHalf,
        records: Arc<Mutex<Records>>,
        token_mode: Arc<Mutex<TokenMode>>,
        write_half: Arc<AsyncMutex<Option<OwnedWriteHalf>>>,
        advertise_credential_get: Arc<Mutex<bool>>,
    ) {
        let mut reader = BufReader::new(read_half);
        let mut line = Vec::new();
        loop {
            line.clear();
            match reader.read_until(b'\n', &mut line).await {
                Ok(0) | Err(_) => return,
                Ok(_) => {}
            }
            let trimmed = strip_trailing_newline(&line);
            if trimmed.is_empty() {
                continue;
            }
            let Ok(msg): Result<Value, _> = serde_json::from_slice(trimmed) else {
                continue;
            };
            match msg.get("type").and_then(|t| t.as_str()) {
                Some("hello") => {
                    records.lock().unwrap().hello_count += 1;
                    let mut ack = json!({
                        "type": "hello_ack", "v": 2,
                        "namespaces": ["ssh-agent", "aws-credentials", "docker-credentials"],
                        "gate_namespaces": ["ssh-agent"],
                        "request_timeout_ms": 25000, "accepted": true,
                    });
                    // An OLD agent omits the key entirely — it is not `[]`, it is
                    // absent, and that absence is the whole signal.
                    if *advertise_credential_get.lock().unwrap() {
                        ack["agent_capabilities"] = json!([CAP_CREDENTIAL_GET]);
                    }
                    send_on(&write_half, ack).await;
                }
                Some("approval_response") => records.lock().unwrap().responses.push(msg),
                Some("token.get") => {
                    let mode = *token_mode.lock().unwrap();
                    let (id, server) = {
                        records.lock().unwrap().token_gets.push(msg.clone());
                        (
                            msg.get("id")
                                .and_then(|v| v.as_str())
                                .unwrap_or("")
                                .to_string(),
                            msg.get("server")
                                .and_then(|v| v.as_str())
                                .unwrap_or("")
                                .to_string(),
                        )
                    };
                    match mode {
                        TokenMode::Silent => {}
                        TokenMode::Error => {
                            send_on(
                                &write_half,
                                json!({"type":"token.response","in_reply_to":id,"server":server,"error":"mint failed"}),
                            )
                            .await;
                        }
                        TokenMode::Ok => {
                            let n = {
                                let mut r = records.lock().unwrap();
                                r.token_seq += 1;
                                r.token_seq
                            };
                            send_on(
                                &write_half,
                                json!({"type":"token.response","in_reply_to":id,"server":server,"token":format!("fake-tok-{n}")}),
                            )
                            .await;
                        }
                    }
                }
                Some("credential.get") => {
                    let (id, server) = {
                        records.lock().unwrap().credential_gets.push(msg.clone());
                        (
                            msg.get("id")
                                .and_then(|v| v.as_str())
                                .unwrap_or("")
                                .to_string(),
                            msg.get("server")
                                .and_then(|v| v.as_str())
                                .unwrap_or("")
                                .to_string(),
                        )
                    };
                    // Answer as an mtls server would: a certificate, no token.
                    send_on(
                        &write_half,
                        json!({"type":"credential.response","in_reply_to":id,"server":server,
                               "auth_mode":"mtls","client_cert":"PEM","cert_serial":"0a0b"}),
                    )
                    .await;
                }
                _ => {}
            }
        }
    }

    async fn send_on(write_half: &Arc<AsyncMutex<Option<OwnedWriteHalf>>>, obj: Value) {
        if let Some(wh) = write_half.lock().await.as_mut() {
            let mut bytes = serde_json::to_vec(&obj).unwrap();
            bytes.push(b'\n');
            let _ = wh.write_all(&bytes).await;
        }
    }

    async fn wait_until(mut cond: impl FnMut() -> bool) -> bool {
        for _ in 0..400 {
            if cond() {
                return true;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        false
    }

    fn clock() -> ClockRef {
        Arc::new(FixedClock)
    }

    fn info() -> HelloClientInfo {
        HelloClientInfo {
            name: "shed-desktop".into(),
            version: "0.0.0".into(),
            pid: 1,
            capabilities: vec!["approval.ssh".into(), "event.stream".into()],
            replay_events: 50,
        }
    }

    #[tokio::test]
    async fn handshake_emits_connected() {
        let agent = TestAgent::start();
        let client = agent.client(clock());
        let mut events = client.start(info());
        match tokio::time::timeout(Duration::from_secs(5), events.recv()).await {
            Ok(Some(HostAgentEvent::Connected(ack))) => {
                assert_eq!(ack.gate_namespaces, vec!["ssh-agent"]);
                assert!(ack.accepted);
            }
            other => panic!("expected Connected, got {other:?}"),
        }
        assert!(client.is_connected());
        client.stop();
    }

    #[tokio::test]
    async fn request_token_when_not_started_is_not_connected() {
        // No run loop -> writer is None -> fail closed.
        let agent = TestAgent::start();
        let client = agent.client(clock());
        let e = client
            .request_token("mini2", Duration::from_millis(200))
            .await
            .unwrap_err();
        assert_eq!(e, HostAgentClientError::NotConnected);
    }

    #[tokio::test]
    async fn respond_when_disconnected_is_noop() {
        // A client pointed at a dead socket never connects; respond is a no-op
        // (no panic), and the agent fails closed on its side.
        let clock = clock();
        let client = HostAgentClient::new("/nonexistent/shed-ha.sock", clock);
        let _events = client.start(info());
        client.respond(
            "rid",
            ApprovalDecision::Approve,
            DecidedBy::User,
            None,
            None,
        );
        assert!(!client.is_connected());
        client.stop();
    }

    #[tokio::test]
    async fn request_token_success() {
        let agent = TestAgent::start();
        let client = agent.client(clock());
        let mut events = client.start(info());
        let _ = tokio::time::timeout(Duration::from_secs(5), events.recv()).await;
        assert!(agent.wait_hello(1).await);
        let resp = client
            .request_token("mini2", DEFAULT_TOKEN_TIMEOUT)
            .await
            .unwrap();
        assert_eq!(resp.token.as_deref(), Some("fake-tok-1"));
        assert_eq!(resp.server, "mini2");
        assert!(resp.error.is_none());
        client.stop();
    }

    #[tokio::test]
    async fn request_token_error_reply_is_returned_not_thrown() {
        // A fail-closed reply (error set, no token) is returned IN the response —
        // the caller (the minter) inspects it; it is not an Err.
        let agent = TestAgent::start();
        agent.set_token_mode(TokenMode::Error);
        let client = agent.client(clock());
        let mut events = client.start(info());
        let _ = tokio::time::timeout(Duration::from_secs(5), events.recv()).await;
        let resp = client
            .request_token("mini2", DEFAULT_TOKEN_TIMEOUT)
            .await
            .unwrap();
        assert_eq!(resp.error.as_deref(), Some("mint failed"));
        assert!(resp.token.is_none());
        client.stop();
    }

    #[tokio::test]
    async fn request_token_times_out_when_silent() {
        let agent = TestAgent::start();
        agent.set_token_mode(TokenMode::Silent);
        let client = agent.client(clock());
        let mut events = client.start(info());
        let _ = tokio::time::timeout(Duration::from_secs(5), events.recv()).await;
        assert!(agent.wait_hello(1).await);
        let e = client
            .request_token("mini2", Duration::from_millis(150))
            .await
            .unwrap_err();
        assert_eq!(e, HostAgentClientError::TimedOut);
        client.stop();
    }

    #[tokio::test]
    async fn disconnect_fails_inflight_token_request() {
        // F10: a drop while a token.get is in flight fails it (Disconnected),
        // not a hang until timeout.
        let agent = TestAgent::start();
        agent.set_token_mode(TokenMode::Silent);
        let client = agent.client(clock());
        let mut events = client.start(info());
        let _ = tokio::time::timeout(Duration::from_secs(5), events.recv()).await;
        assert!(agent.wait_hello(1).await);
        let c2 = client.clone();
        let req =
            tokio::spawn(async move { c2.request_token("mini2", Duration::from_secs(30)).await });
        assert!(agent.wait_token_gets(1).await);
        agent.drop_conn().await;
        let e = tokio::time::timeout(Duration::from_secs(5), req)
            .await
            .unwrap()
            .unwrap()
            .unwrap_err();
        assert_eq!(e, HostAgentClientError::Disconnected);
        client.stop();
    }

    #[tokio::test]
    async fn reconnects_after_drop() {
        let agent = TestAgent::start();
        let client = agent.client(clock());
        let _events = client.start(info());
        assert!(agent.wait_hello(1).await);
        agent.drop_conn().await;
        // The client's backoff-reconnect re-handshakes.
        assert!(agent.wait_hello(2).await, "client did not reconnect");
        client.stop();
    }

    #[tokio::test]
    async fn late_reply_after_timeout_is_ignored() {
        let agent = TestAgent::start();
        agent.set_token_mode(TokenMode::Silent);
        let client = agent.client(clock());
        let mut events = client.start(info());
        let _ = tokio::time::timeout(Duration::from_secs(5), events.recv()).await;
        assert!(agent.wait_hello(1).await);
        let e = client
            .request_token("mini2", Duration::from_millis(120))
            .await
            .unwrap_err();
        assert_eq!(e, HostAgentClientError::TimedOut);
        // A stray, late token.response for the timed-out request must not panic
        // or corrupt state.
        let stray_id = agent.token_gets()[0]["id"].as_str().unwrap().to_string();
        agent
            .write_frame(json!({"type":"token.response","in_reply_to":stray_id,"server":"mini2","token":"stale"}))
            .await;
        // A subsequent request still works — proving state wasn't corrupted.
        agent.set_token_mode(TokenMode::Ok);
        let resp = client
            .request_token("mini2", DEFAULT_TOKEN_TIMEOUT)
            .await
            .unwrap();
        assert_eq!(resp.token.as_deref(), Some("fake-tok-1"));
        client.stop();
    }

    #[tokio::test]
    async fn duplicate_reply_after_success_is_ignored() {
        let agent = TestAgent::start();
        let client = agent.client(clock());
        let mut events = client.start(info());
        let _ = tokio::time::timeout(Duration::from_secs(5), events.recv()).await;
        assert!(agent.wait_hello(1).await);
        let resp = client
            .request_token("mini2", DEFAULT_TOKEN_TIMEOUT)
            .await
            .unwrap();
        let id = agent.token_gets()[0]["id"].as_str().unwrap().to_string();
        // A duplicate reply for the already-resolved request is a no-op.
        agent
            .write_frame(
                json!({"type":"token.response","in_reply_to":id,"server":"mini2","token":"dup"}),
            )
            .await;
        assert_eq!(resp.token.as_deref(), Some("fake-tok-1"));
        // Client still functional.
        let resp2 = client
            .request_token("mini3", DEFAULT_TOKEN_TIMEOUT)
            .await
            .unwrap();
        assert_eq!(resp2.token.as_deref(), Some("fake-tok-2"));
        client.stop();
    }

    #[tokio::test]
    async fn unknown_in_reply_to_is_ignored() {
        let agent = TestAgent::start();
        let client = agent.client(clock());
        let mut events = client.start(info());
        let _ = tokio::time::timeout(Duration::from_secs(5), events.recv()).await;
        assert!(agent.wait_hello(1).await);
        // A token.response with no matching pending request must be a no-op.
        agent
            .write_frame(
                json!({"type":"token.response","in_reply_to":"nobody","server":"x","token":"t"}),
            )
            .await;
        tokio::time::sleep(Duration::from_millis(50)).await;
        let resp = client
            .request_token("mini2", DEFAULT_TOKEN_TIMEOUT)
            .await
            .unwrap();
        assert_eq!(resp.token.as_deref(), Some("fake-tok-1"));
        client.stop();
    }

    #[tokio::test]
    async fn stop_while_pending_fails_request() {
        let agent = TestAgent::start();
        agent.set_token_mode(TokenMode::Silent);
        let client = agent.client(clock());
        let mut events = client.start(info());
        let _ = tokio::time::timeout(Duration::from_secs(5), events.recv()).await;
        assert!(agent.wait_hello(1).await);
        let c2 = client.clone();
        let req =
            tokio::spawn(async move { c2.request_token("mini2", Duration::from_secs(30)).await });
        assert!(agent.wait_token_gets(1).await);
        client.stop();
        let e = tokio::time::timeout(Duration::from_secs(5), req)
            .await
            .unwrap()
            .unwrap()
            .unwrap_err();
        assert_eq!(e, HostAgentClientError::Disconnected);
    }

    #[tokio::test]
    async fn respond_writes_approval_response() {
        let agent = TestAgent::start();
        let client = agent.client(clock());
        let mut events = client.start(info());
        let _ = tokio::time::timeout(Duration::from_secs(5), events.recv()).await;
        assert!(agent.wait_hello(1).await);
        client.respond(
            "rid-1",
            ApprovalDecision::Approve,
            DecidedBy::User,
            Some("per-session"),
            Some("1h"),
        );
        assert!(agent.wait_responses(1).await);
        let r = &agent.responses()[0];
        assert_eq!(r["request_id"], "rid-1");
        assert_eq!(r["decision"], "approve");
        assert_eq!(r["decided_by"], "user");
        assert_eq!(r["scope"], "per-session");
        assert_eq!(r["ttl"], "1h");
        client.stop();
    }

    #[tokio::test]
    async fn partial_frame_at_eof_is_dropped_not_processed() {
        // A token.response WITHOUT its trailing newline, followed by a close, must
        // be dropped (treated as disconnect) — never decoded into a usable token.
        let agent = TestAgent::start();
        agent.set_token_mode(TokenMode::Silent);
        let client = agent.client(clock());
        let mut events = client.start(info());
        let _ = tokio::time::timeout(Duration::from_secs(5), events.recv()).await;
        assert!(agent.wait_hello(1).await);
        let c2 = client.clone();
        let req =
            tokio::spawn(async move { c2.request_token("mini2", Duration::from_secs(30)).await });
        assert!(agent.wait_token_gets(1).await);
        let id = agent.token_gets()[0]["id"].as_str().unwrap().to_string();
        let partial = serde_json::to_vec(
            &json!({"type":"token.response","in_reply_to":id,"server":"mini2","token":"leaked"}),
        )
        .unwrap();
        agent.write_raw(&partial).await; // no trailing newline
        agent.drop_conn().await;
        let e = tokio::time::timeout(Duration::from_secs(5), req)
            .await
            .unwrap()
            .unwrap()
            .unwrap_err();
        assert_eq!(e, HostAgentClientError::Disconnected); // NOT Ok("leaked")
        client.stop();
    }

    #[test]
    fn a1_peer_trusted_requires_matching_uid() {
        // A1: the peer must run as us; a lookup error (None) fails closed.
        assert!(peer_trusted(Some(1000), 1000));
        assert!(!peer_trusted(Some(1001), 1000));
        assert!(!peer_trusted(None, 1000));
    }

    #[tokio::test]
    async fn a1_peer_uid_of_a_local_socket_is_ours() {
        // Both ends of an in-process socket are us → the real getpeereid path
        // returns our uid and the trust check passes (the happy side; a wrong-UID
        // peer needs privileges to bind, so that case is the pure test above).
        let path = std::env::temp_dir().join(format!("shed-peeruid-{}.sock", new_id()));
        let _ = std::fs::remove_file(&path);
        let listener = tokio::net::UnixListener::bind(&path).unwrap();
        let accept = tokio::spawn(async move { listener.accept().await.map(|(s, _)| s) });
        let client = UnixStream::connect(&path).await.unwrap();
        let _server = accept.await.unwrap().unwrap();
        assert_eq!(peer_uid(&client), Some(our_uid()));
        assert!(peer_trusted(peer_uid(&client), our_uid()));
        let _ = std::fs::remove_file(&path);
    }

    // -----------------------------------------------------------------------
    // Plan 002 §7 P9 — the shared, language-neutral desktop-credential vectors.
    //
    // These are the SAME files the Swift decoder tests
    // (`desktop/Tests/ShedKitTests/HostAgentCredentialFixtureTests.swift`) and
    // the Go/Rust AGENT suites read. The point is not that each language has a
    // test — it is that a divergence in how Go, Rust and Swift read one byte
    // string fails a test, per commit, instead of surfacing as a field report.
    // -----------------------------------------------------------------------

    fn fixture(name: &str) -> Value {
        let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../tests/host-agent-diff/fixtures/desktop-credential")
            .join(name);
        let raw = std::fs::read_to_string(&path)
            .unwrap_or_else(|e| panic!("read fixture {}: {e}", path.display()));
        serde_json::from_str(&raw).expect("fixture is valid JSON")
    }

    /// Every `hello_ack` vector, through the PRODUCTION decoder and the
    /// PRODUCTION capability derivation (`Inner::apply_hello_ack`).
    ///
    /// `unknown` is deliberately unreachable from any frame: it is the pre-ack
    /// state, asserted separately below, and conflating it with `unsupported` is
    /// the §7 P5 bug these vectors exist to prevent.
    #[test]
    fn golden_hello_ack_capability() {
        let fx = fixture("hello_ack.json");
        assert_eq!(fx["protocol_version"], 1, "fixture version skew");
        let vectors = fx["vectors"].as_array().expect("vectors");
        assert!(!vectors.is_empty());
        for v in vectors {
            let name = v["name"].as_str().unwrap();
            let line = serde_json::to_vec(&v["frame"]).unwrap();
            let ack = match protocol::decode(&line).unwrap() {
                HostAgentInbound::HelloAck(a) => a,
                other => panic!("{name}: expected hello_ack, got {other:?}"),
            };
            let client = HostAgentClient::new("/nonexistent.sock", Arc::new(FixedClock));
            assert_eq!(
                client.credential_capability(),
                AgentCapabilityState::Unknown,
                "{name}: a fresh connection has learned nothing"
            );
            client.inner.apply_hello_ack(&ack);
            let want = match v["expected_capability"].as_str().unwrap() {
                "supported" => AgentCapabilityState::Supported,
                "unsupported" => AgentCapabilityState::Unsupported,
                other => panic!("{name}: unexpected fixture capability {other:?}"),
            };
            assert_eq!(client.credential_capability(), want, "{name}");
        }
    }

    /// A REJECTED ack teaches nothing: the agent declined this client, so the
    /// list it sent describes a session we do not have. Not a fixture vector —
    /// every vector is an accepted ack — but the same derivation.
    #[test]
    fn a_rejected_hello_ack_leaves_the_capability_unknown() {
        let line =
            br#"{"type":"hello_ack","agent_capabilities":["credential.get"],"accepted":false}"#;
        let ack = match protocol::decode(line).unwrap() {
            HostAgentInbound::HelloAck(a) => a,
            other => panic!("expected hello_ack, got {other:?}"),
        };
        let client = HostAgentClient::new("/nonexistent.sock", Arc::new(FixedClock));
        client.inner.apply_hello_ack(&ack);
        assert_eq!(
            client.credential_capability(),
            AgentCapabilityState::Unknown
        );
    }

    /// Every `credential.get` vector, through the PRODUCTION frame builder: the
    /// exact key set (a CSR-less request OMITS the key rather than sending
    /// `""`, which is what makes an mtls server's refusal legible), the values,
    /// and the §7 P3 key-containment assertion over the bytes actually written.
    #[test]
    fn golden_credential_get_frame() {
        let fx = fixture("credential_get.json");
        assert_eq!(fx["protocol_version"], 1, "fixture version skew");
        let forbidden: Vec<String> = fx["forbidden_substrings"]
            .as_array()
            .expect("forbidden_substrings")
            .iter()
            .map(|s| s.as_str().unwrap().to_lowercase())
            .collect();
        assert!(!forbidden.is_empty());
        let vectors = fx["vectors"].as_array().expect("vectors");
        assert!(!vectors.is_empty());
        for v in vectors {
            let name = v["name"].as_str().unwrap();
            let server = v["request"]["server"].as_str().unwrap();
            let csr = v["request"]["csr"].as_str();
            // Passed through VERBATIM (including the empty-string vector): the
            // builder owns the omit-when-empty rule, so this asserts it rather
            // than reimplementing it.
            let line = protocol::credential_get("req-id", server, csr);
            let got: Value = serde_json::from_str(&line).unwrap();

            let mut keys: Vec<&str> = got
                .as_object()
                .unwrap()
                .keys()
                .map(String::as_str)
                .collect();
            keys.sort_unstable();
            let mut want_keys: Vec<&str> = v["expected_keys"]
                .as_array()
                .unwrap()
                .iter()
                .map(|k| k.as_str().unwrap())
                .collect();
            want_keys.sort_unstable();
            assert_eq!(keys, want_keys, "{name}: key set");

            // `id` is a per-request UUID — presence, not value.
            let mut want = v["expected_frame"].clone();
            want["id"] = got["id"].clone();
            assert_eq!(got, want, "{name}: frame");

            let lowered = line.to_lowercase();
            for marker in &forbidden {
                assert!(
                    !lowered.contains(marker),
                    "{name}: app→agent frame carries private-key marker {marker:?}"
                );
            }
        }
    }

    /// The outbound half of the size caps: an oversized CSR is refused rather
    /// than written. `max_csr_bytes` is the fixture's, so the cap cannot drift
    /// from what the other clients enforce.
    #[tokio::test]
    async fn oversized_csr_is_refused_before_it_is_written() {
        let fx = fixture("credential_get.json");
        let cap = fx["max_csr_bytes"].as_u64().unwrap() as usize;
        assert_eq!(cap, crate::token_minter::limits::MAX_CSR_BYTES);
        let agent = TestAgent::start();
        agent.advertise_credential_get();
        let client = agent.client(Arc::new(FixedClock));
        let _events = client.start(HelloClientInfo {
            name: "t".into(),
            version: "1".into(),
            pid: 1,
            capabilities: vec![],
            replay_events: 0,
        });
        assert!(agent.wait_hello(1).await);
        assert!(wait_until(|| client.supports(CAP_CREDENTIAL_GET)).await);
        let huge = "A".repeat(cap + 1);
        let err = client
            .request_credential(
                "mini2",
                Some(&huge),
                client.credential_capability_snapshot(),
                Duration::from_secs(2),
            )
            .await
            .unwrap_err();
        assert_eq!(err, HostAgentClientError::OversizedCsr(cap + 1));
        assert!(
            agent.credential_gets().is_empty(),
            "an over-cap CSR must never reach the socket"
        );
        client.stop();
    }

    /// The generation binding (§7 P5): a snapshot taken on one connection cannot
    /// be spent on the next one. A reconnect between the decision and the send
    /// is refused as `CapabilityLost` — "try again", not "upgrade something".
    #[tokio::test]
    async fn a_capability_snapshot_from_a_previous_connection_is_refused() {
        let agent = TestAgent::start();
        agent.advertise_credential_get();
        let client = agent.client(Arc::new(FixedClock));
        let _events = client.start(HelloClientInfo {
            name: "t".into(),
            version: "1".into(),
            pid: 1,
            capabilities: vec![],
            replay_events: 0,
        });
        assert!(agent.wait_hello(1).await);
        assert!(wait_until(|| client.supports(CAP_CREDENTIAL_GET)).await);
        let stale = client.credential_capability_snapshot();
        agent.drop_conn().await;
        assert!(agent.wait_hello(2).await);
        assert!(
            wait_until(|| client.credential_capability_snapshot().generation != stale.generation)
                .await
        );
        let err = client
            .request_credential("mini2", Some("QUJD"), stale, Duration::from_millis(500))
            .await
            .unwrap_err();
        assert_eq!(err, HostAgentClientError::CapabilityLost);
        client.stop();
    }

    /// The pre-ack wait: `await_credential_capability` returns as soon as the
    /// ack lands, and reports the still-unknown state on timeout rather than
    /// inventing an answer.
    #[tokio::test]
    async fn await_capability_resolves_on_the_ack_and_times_out_unknown() {
        // No agent at all → nothing to learn, and the wait must END.
        let client = HostAgentClient::new("/nonexistent-shed-agent.sock", Arc::new(FixedClock));
        let snapshot = client
            .await_credential_capability(Duration::from_millis(150))
            .await;
        assert_eq!(snapshot.state, AgentCapabilityState::Unknown);

        let agent = TestAgent::start();
        agent.advertise_credential_get();
        let client = agent.client(Arc::new(FixedClock));
        let _events = client.start(HelloClientInfo {
            name: "t".into(),
            version: "1".into(),
            pid: 1,
            capabilities: vec![],
            replay_events: 0,
        });
        let snapshot = client
            .await_credential_capability(Duration::from_secs(5))
            .await;
        assert_eq!(snapshot.state, AgentCapabilityState::Supported);
        client.stop();
    }

    /// Plan 002 §7 P5's live-agent rows, driven through the REAL minter (the
    /// pre-ack rows live in `token_minter.rs`, which needs no agent at all).
    #[tokio::test]
    async fn minter_capability_table_against_a_live_agent() {
        use crate::auth_modes::AuthModeRegistry;
        use crate::token_minter::HostAgentTokenMinter;
        use shed_core::token::{CredentialRequest, MintedCredential, TokenMinter};

        let hello = || HelloClientInfo {
            name: "t".into(),
            version: "1".into(),
            pid: 1,
            capabilities: vec![],
            replay_events: 0,
        };

        // --- unsupported + expects mtls: the honest "upgrade shed-host-agent". ---
        let agent = TestAgent::start();
        let client = agent.client(Arc::new(FixedClock));
        let _events = client.start(hello());
        assert!(agent.wait_hello(1).await);
        assert!(
            wait_until(|| client.credential_capability() == AgentCapabilityState::Unsupported)
                .await
        );
        let modes = Arc::new(AuthModeRegistry::new());
        modes.record("mini2", shed_core::token::AuthMode::Mtls);
        let minter = HostAgentTokenMinter::new(client.clone()).with_modes(modes.clone());
        assert!(
            !minter.supports_mtls(),
            "an agent that advertised nothing cannot relay a CSR"
        );
        let e = minter
            .mint_credential("mini2", &CredentialRequest::default())
            .await
            .unwrap_err();
        assert!(
            format!("{e:?}").contains("upgrade shed-host-agent"),
            "an OLD agent + an mtls server is a real upgrade case: {e:?}"
        );
        assert!(agent.credential_gets().is_empty());
        client.stop();

        // --- unsupported + token server: unchanged, forever. ---
        let agent = TestAgent::start();
        let client = agent.client(Arc::new(FixedClock));
        let _events = client.start(hello());
        assert!(agent.wait_hello(1).await);
        assert!(
            wait_until(|| client.credential_capability() == AgentCapabilityState::Unsupported)
                .await
        );
        let modes = Arc::new(AuthModeRegistry::new());
        let minter = HostAgentTokenMinter::new(client.clone()).with_modes(modes.clone());
        match minter
            .mint_credential("mini2", &CredentialRequest::default())
            .await
            .unwrap()
        {
            MintedCredential::Token(t) => assert_eq!(t.token, "fake-tok-1"),
            other => panic!("expected the legacy token.get, got {other:?}"),
        }
        assert!(agent.credential_gets().is_empty());
        client.stop();

        // --- supported: the CSR crosses, the certificate comes back, and the
        //     minter records the learned mode SYNCHRONOUSLY (the observer would
        //     only get there after the core adopts). ---
        let agent = TestAgent::start();
        agent.advertise_credential_get();
        let client = agent.client(Arc::new(FixedClock));
        let _events = client.start(hello());
        assert!(agent.wait_hello(1).await);
        assert!(wait_until(|| client.supports(CAP_CREDENTIAL_GET)).await);
        let modes = Arc::new(AuthModeRegistry::new());
        let minter = HostAgentTokenMinter::new(client.clone()).with_modes(modes.clone());
        assert!(minter.supports_mtls());
        match minter
            .mint_credential("mini2", &CredentialRequest::with_csr("QUJD"))
            .await
            .unwrap()
        {
            MintedCredential::Certificate(c) => assert_eq!(c.cert_pem, "PEM"),
            other => panic!("expected a certificate, got {other:?}"),
        }
        assert_eq!(agent.credential_gets()[0]["csr"], "QUJD");
        assert!(
            modes.expects_mtls("mini2"),
            "the minter must record the mode it just saw, without waiting for the observer"
        );
        client.stop();
    }

    /// The desktop↔host-agent compat matrix, from the APP's side.
    #[tokio::test]
    async fn credential_get_compat_matrix() {
        // --- NEW app, OLD agent: no capability advertised. ---
        let agent = TestAgent::start();
        let client = agent.client(Arc::new(FixedClock));
        let _events = client.start(HelloClientInfo {
            name: "t".into(),
            version: "1".into(),
            pid: 1,
            capabilities: vec![],
            replay_events: 0,
        });
        assert!(agent.wait_hello(1).await);
        assert!(
            !client.supports(CAP_CREDENTIAL_GET),
            "an agent that advertised nothing must not appear to support anything"
        );
        // The ack HAS landed, so the tri-state is a real answer (Unsupported),
        // not the pre-ack Unknown.
        assert!(
            wait_until(|| client.credential_capability() == AgentCapabilityState::Unsupported)
                .await
        );
        let err = client
            .request_credential(
                "mini2",
                Some("QUJD"),
                client.credential_capability_snapshot(),
                Duration::from_millis(200),
            )
            .await
            .unwrap_err();
        assert_eq!(err, HostAgentClientError::Unsupported(CAP_CREDENTIAL_GET));
        // The message has to name what to upgrade — that is the entire reason the
        // capability exists instead of a timeout.
        assert!(
            err.to_string().contains("upgrade shed-host-agent"),
            "unhelpful mismatch error: {err}"
        );
        // And nothing was written: an old agent must never receive a frame it
        // would silently drop.
        assert!(agent.credential_gets().is_empty());
        // token.get against the same old agent still works untouched.
        let resp = client
            .request_token("mini2", Duration::from_secs(2))
            .await
            .unwrap();
        assert_eq!(resp.token.as_deref(), Some("fake-tok-1"));
        client.stop();

        // --- NEW app, NEW agent: the CSR crosses and a certificate comes back. ---
        let agent = TestAgent::start();
        agent.advertise_credential_get();
        let client = agent.client(Arc::new(FixedClock));
        let _events = client.start(HelloClientInfo {
            name: "t".into(),
            version: "1".into(),
            pid: 1,
            capabilities: vec![],
            replay_events: 0,
        });
        assert!(agent.wait_hello(1).await);
        assert!(wait_until(|| client.supports(CAP_CREDENTIAL_GET)).await);
        let resp = client
            .request_credential(
                "mini2",
                Some("QUJD"),
                client.credential_capability_snapshot(),
                Duration::from_secs(2),
            )
            .await
            .unwrap();
        assert_eq!(resp.auth_mode.as_deref(), Some("mtls"));
        assert_eq!(resp.client_cert.as_deref(), Some("PEM"));
        assert!(resp.token.is_none());
        let sent = agent.credential_gets();
        assert_eq!(sent.len(), 1);
        assert_eq!(sent[0]["csr"], "QUJD", "the CSR must cross verbatim");
        assert_eq!(sent[0]["server"], "mini2");
        client.stop();
    }
}
