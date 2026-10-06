//! One NDJSON connection to craze: line framing with craze's limits from the
//! CLIENT's side, request/response demux by id, notification dispatch, the
//! bounded preamble, and the hub `hello` (plan 025 §3.3.1).
//!
//! # Framing
//!
//! One JSON object per line, `\n`-terminated, a `\r` before it tolerated (PM
//! "Framing"). The limits are craze's, turned around:
//!
//! - **This client never writes a line over [`INBOUND_LINE_MAX`]** (4 MiB of
//!   content). A host would discard it and answer `-32600` with `id: null`,
//!   which cannot be matched to the request it lost; so the request is refused
//!   here, [`CallError::TooLong`], and nothing is written.
//! - **It reads lines up to [`OUTBOUND_LINE_MAX`]** (16 MiB of content), the
//!   most any host or hub writes. A longer one is a protocol fault that ends
//!   the connection — nothing craze sends is that long, so whatever is on the
//!   other end is not craze.
//!
//! # Demux
//!
//! Requests carry string ids counted from `"1"`; a reply is filed by its id —
//! the JSON value exactly as craze echoes it, so a numeric `1` never answers
//! the string `"1"` — whatever order replies come in: a host answers up to 16
//! requests per connection out of order (craze
//! `internal/protocol/limits.go:47-50`, a behaviour craze#89 item 5 notes
//! protocol.md does not state). A reply nobody waits for (its request's
//! deadline passed) is dropped. Every message must say `"jsonrpc":"2.0"`.
//!
//! A notification goes to the connection's [`Notifications`] channel, bounded
//! at [`NOTIFICATION_QUEUE`] — and **the reader never waits on it**. One reader
//! files both replies and notifications, so a reader that waited for a slow
//! consumer would hold every reply queued behind the notification it is
//! stuck on, and a caller that awaits a reply while it drains notifications
//! (the lane's watcher, attaching while events stream in) would deadlock. A full
//! queue ends the connection instead, as [`ConnEnd::Backlog`]: a source treats
//! it as a lost connection (redial, reseed), a lane as correction 13's
//! `Lagged` (drain, then reseed). What the reader DOES do, once the queue is
//! half full, is yield its turn after each notification: a burst already in its
//! buffer (a replay, a roster flush) would otherwise be filed in one go,
//! faster than any consumer is scheduled, and end a healthy connection.
//!
//! **Every deadline covers the whole exchange** — the write, its flush and the
//! reply — so a peer that stops reading cannot hold a request past it. A write
//! cut off by its deadline may have left half a line on the wire, so the
//! connection is ended with it.
//!
//! # The bounded preamble
//!
//! A shed's sshd runs every command through `bash -lc`, so a noisy login profile
//! can print before craze does (shed#231's class). Before the connection's
//! FIRST reply, up to [`PREAMBLE_MAX_LINES`] non-JSON lines totalling at most
//! [`PREAMBLE_MAX_BYTES`] are skipped — and kept, so a failure before `hello`
//! can quote them ([`Conn::preamble`]). The next one past either bound ends the
//! connection, and so does any non-JSON line after the first reply — a blank or
//! whitespace-only one included.
//!
//! # The hub `hello`
//!
//! [`hello_hub`] says `hello` as `{kind: "shed", name, version}` and judges the
//! answer: protocol 1 from a `hub`, **`codecs.event == 1 && codecs.snapshot ==
//! 1`** (PM "Versioning": a client must not fold a codec it does not know), and
//! the two capabilities the source cannot work without, `rosterSubscribe` and
//! `connect` — anything short of that is [`HelloError::TooOld`]. A refusal of
//! `hello` for want of a shared protocol (`bad_request`/`protocol_version`) is
//! `TooOld` too; any other refusal is [`HelloError::Refused`]. The host
//! `hello` the lane says through the splice ([`judge_host_hello`]) is held to
//! the same protocol and codec rule, from an `endpoint.kind` of `"host"`.
//!
//! # Ending a connection from outside
//!
//! [`Conn::close`] ends a live connection on purpose: every waiter fails, the
//! notifications end, and a process behind it is killed. The lane uses it when
//! a verb's deadline passes with no reply — a half-open transport shows up
//! exactly there, since nothing keeps the loopback or exec stream alive — so
//! its watcher sees the connection gone and reconnects.

use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;

use serde::{Deserialize, Serialize};
use serde_json::Value;
use shed_core::lane::feed::truncate_bytes;
use tokio::io::{AsyncBufReadExt, AsyncRead, AsyncWriteExt, BufReader};
use tokio::sync::mpsc::error::TrySendError;
use tokio::sync::{mpsc, oneshot};
use tokio::task::JoinHandle;

use crate::dial::{BoxWrite, CrazeStream, ExitWatch, StderrTail};
use crate::wire::{
    self, code, method, reason, ClientInfo, ConnCapabilities, HelloParams, HelloResult, Incoming,
    LineError, RpcError, CODEC_EVENT, CODEC_SNAPSHOT, INBOUND_LINE_MAX, OUTBOUND_LINE_MAX,
    PROTOCOL,
};

/// How many non-JSON lines may precede the first reply (plan 025 §3.3.2).
pub const PREAMBLE_MAX_LINES: usize = 16;
/// How many bytes of them (their content, newlines not counted).
pub const PREAMBLE_MAX_BYTES: usize = 4 << 10;

/// How many notifications may wait for the consumer. One more ends the
/// connection as [`ConnEnd::Backlog`] — the reader never waits for room. As
/// deep as the contract's own frame channel (`LANE_CHANNEL_CAPACITY`), so a
/// burst the consumer can publish onward is a burst this queue can hold.
pub const NOTIFICATION_QUEUE: usize = shed_core::lane::LANE_CHANNEL_CAPACITY;

/// A notification, as a peer sent it.
#[derive(Debug, Clone, PartialEq)]
pub struct Notification {
    pub method: String,
    pub params: Value,
}

/// A connection's notifications, in arrival order. It yields `None` once the
/// connection has ended — [`Conn::ended`] then says why.
pub type Notifications = mpsc::Receiver<Notification>;

/// Why a connection ended.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ConnEnd {
    /// The peer closed it, a read or a write failed or stalled past its
    /// deadline, or the peer wrote something that is not protocol 1 — the
    /// text says which.
    Lost(String),
    /// The consumer left [`NOTIFICATION_QUEUE`] notifications undrained, and
    /// the reader ended the connection rather than stall the replies behind
    /// them (the module doc).
    Backlog,
}

impl std::fmt::Display for ConnEnd {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ConnEnd::Lost(why) => f.write_str(why),
            ConnEnd::Backlog => write!(
                f,
                "more than {NOTIFICATION_QUEUE} notifications from craze went unread; the connection was dropped rather than stall its replies"
            ),
        }
    }
}

/// What one [`Conn::request`] can come to besides its result.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum CallError {
    /// A JSON-RPC error reply: the peer answered, definitely, with a refusal.
    /// (Boxed: a refusal is rare, and an `Err` this wide would cost every
    /// `Result` that carries one.)
    #[error("{0}")]
    Refused(Box<RpcError>),
    /// The connection ended before the reply came. Whether the request ran is
    /// UNKNOWN.
    #[error("{0}")]
    Ended(String),
    /// No reply within the deadline. Whether the request ran is UNKNOWN.
    #[error("no answer from craze within {0:?}")]
    Deadline(Duration),
    /// The request's line is over [`INBOUND_LINE_MAX`]; it was not written.
    #[error("the request is {bytes} bytes, over craze's {INBOUND_LINE_MAX}-byte line limit; it was not sent")]
    TooLong { bytes: usize },
    /// The params did not serialize; nothing was written.
    #[error("the request could not be encoded: {0}")]
    Encode(String),
}

impl CallError {
    /// Whether the request may have run: the connection dropped, or the
    /// deadline passed, after it was written.
    pub fn outcome_unknown(&self) -> bool {
        matches!(self, CallError::Ended(_) | CallError::Deadline(_))
    }
}

/// The reader's ledger: who waits for which reply (keyed by the request id's
/// compact JSON, so `"1"` and `1` are different keys), why the connection
/// ended, and what the preamble held.
#[derive(Default)]
struct State {
    pending: HashMap<String, oneshot::Sender<Result<Value, RpcError>>>,
    ended: Option<ConnEnd>,
    preamble: Vec<String>,
}

impl State {
    /// End the connection (the first reason wins) and fail every waiter —
    /// each learns the end through its dropped sender.
    fn end(&mut self, why: ConnEnd) {
        if self.ended.is_none() {
            self.ended = Some(why);
        }
        self.pending.clear();
    }
}

type Shared = Arc<Mutex<State>>;

/// Every lock in this crate, poison-tolerant. Each guards a ledger a panic
/// cannot leave half-written (a map entry, a buffer, a flag), so a poisoned
/// lock is still usable.
pub(crate) fn lock<T>(m: &Mutex<T>) -> MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|p| p.into_inner())
}

/// One live connection. Dropping it ends it: the reader stops, the write half
/// closes (a bridge's stdin ends), and a process behind it is killed once the
/// last [`ExitWatch`] for it is gone.
pub struct Conn {
    writer: tokio::sync::Mutex<BoxWrite>,
    shared: Shared,
    next_id: AtomicU64,
    reader: JoinHandle<()>,
    stderr: Option<StderrTail>,
    exit: Option<ExitWatch>,
}

impl Drop for Conn {
    fn drop(&mut self) {
        self.reader.abort();
    }
}

impl Conn {
    /// Start reading `stream` and hand back the connection and its
    /// notifications.
    pub fn start(stream: CrazeStream) -> (Conn, Notifications) {
        let (reader, writer, stderr, exit) = stream.into_parts();
        let shared: Shared = Arc::default();
        let (tx, rx) = mpsc::channel(NOTIFICATION_QUEUE);
        let task = tokio::spawn(read_loop(reader, Arc::clone(&shared), tx));
        let conn = Conn {
            writer: tokio::sync::Mutex::new(writer),
            shared,
            next_id: AtomicU64::new(1),
            reader: task,
            stderr,
            exit,
        };
        (conn, rx)
    }

    /// Send one request and wait for its reply — the write, its flush and the
    /// reply all within `deadline` (the module doc).
    pub async fn request<P: Serialize>(
        &self,
        method: &str,
        params: &P,
        deadline: Duration,
    ) -> Result<Value, CallError> {
        let id = self.next_id.fetch_add(1, Ordering::Relaxed).to_string();
        let mut line = wire::request_line(&id, method, params)
            .map_err(|e| CallError::Encode(e.to_string()))?;
        if line.len() > INBOUND_LINE_MAX {
            return Err(CallError::TooLong { bytes: line.len() });
        }
        line.push(b'\n');
        // The id as the JSON value it is on the wire — what a reply must echo.
        let key = Value::String(id).to_string();
        let (tx, rx) = oneshot::channel();
        {
            let mut state = lock(&self.shared);
            if let Some(why) = &state.ended {
                return Err(CallError::Ended(why.to_string()));
            }
            state.pending.insert(key.clone(), tx);
        }
        let wrote = AtomicBool::new(false);
        let exchange = async {
            let written = async {
                let mut w = self.writer.lock().await;
                w.write_all(&line).await?;
                w.flush().await
            };
            if let Err(e) = written.await {
                return Err(CallError::Ended(format!("writing to craze failed: {e}")));
            }
            wrote.store(true, Ordering::Relaxed);
            match rx.await {
                // The reader dropped every waiter when the connection ended.
                Err(_) => Err(CallError::Ended(self.ended_text())),
                Ok(Ok(result)) => Ok(result),
                Ok(Err(refusal)) => Err(CallError::Refused(Box::new(refusal))),
            }
        };
        let outcome = match tokio::time::timeout(deadline, exchange).await {
            Ok(outcome) => outcome,
            Err(_) => {
                if !wrote.load(Ordering::Relaxed) {
                    // Half a line may be on the wire: nothing after it on this
                    // connection could be read as meant.
                    lock(&self.shared).end(ConnEnd::Lost(format!(
                        "a write to craze did not finish within {deadline:?}"
                    )));
                }
                Err(CallError::Deadline(deadline))
            }
        };
        if outcome.is_err() {
            lock(&self.shared).pending.remove(&key);
        }
        outcome
    }

    /// Why the connection ended — `None` while it is live.
    pub fn ended(&self) -> Option<ConnEnd> {
        lock(&self.shared).ended.clone()
    }

    fn ended_text(&self) -> String {
        self.ended().map_or_else(
            || "the connection to craze closed".to_string(),
            |e| e.to_string(),
        )
    }

    /// The non-JSON lines skipped before the first reply, in order.
    pub fn preamble(&self) -> Vec<String> {
        lock(&self.shared).preamble.clone()
    }

    /// The process's stderr, when the dial has one.
    pub fn stderr(&self) -> Option<&StderrTail> {
        self.stderr.as_ref()
    }

    /// The process's exit, when the dial has one.
    pub fn exit(&self) -> Option<&ExitWatch> {
        self.exit.as_ref()
    }

    /// End the connection now, for `why` (the module doc): every waiter fails
    /// with it, the reader stops — so the notifications end once the consumer
    /// has the ones already queued — and the process behind it is killed. A
    /// connection that already ended keeps its first reason.
    pub fn close(&self, why: &str) {
        lock(&self.shared).end(ConnEnd::Lost(why.to_string()));
        // The reader holds the notifications' sender: aborting it is what ends
        // them. It holds no lock across an await, so the abort leaves nothing
        // half-written.
        self.reader.abort();
        if let Some(exit) = &self.exit {
            exit.kill();
        }
    }
}

/// Lines off a byte stream, bounded at `max` bytes of content each.
pub(crate) struct LineReader<R> {
    inner: BufReader<R>,
    max: usize,
    buf: Vec<u8>,
}

/// Why [`LineReader::next_line`] stopped short of a line.
#[derive(Debug)]
pub(crate) enum FrameError {
    TooLong { max: usize },
    Io(std::io::Error),
}

impl std::fmt::Display for FrameError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            FrameError::TooLong { max } => {
                write!(
                    f,
                    "craze wrote a line over {max} bytes, more than craze ever writes"
                )
            }
            FrameError::Io(e) => write!(f, "reading from craze failed: {e}"),
        }
    }
}

impl<R: AsyncRead + Unpin> LineReader<R> {
    pub(crate) fn new(r: R, max: usize) -> LineReader<R> {
        LineReader {
            inner: BufReader::with_capacity(64 << 10, r),
            max,
            buf: Vec::new(),
        }
    }

    /// The next line without its `\n` (and a `\r` before it), or `None` at
    /// EOF. A last line the stream ends without a `\n` is still a line, as
    /// craze's own reader takes it. A line whose content is over `max` is
    /// [`FrameError::TooLong`] — and is never buffered whole: the reader stops
    /// at `max + 1` bytes (a `\r` may still follow).
    pub(crate) async fn next_line(&mut self) -> Result<Option<Vec<u8>>, FrameError> {
        loop {
            let available = self.inner.fill_buf().await.map_err(FrameError::Io)?;
            if available.is_empty() {
                if self.buf.is_empty() {
                    return Ok(None);
                }
                return self.finish().map(Some);
            }
            if let Some(pos) = available.iter().position(|&b| b == b'\n') {
                self.buf.extend_from_slice(&available[..pos]);
                self.inner.consume(pos + 1);
                return self.finish().map(Some);
            }
            let n = available.len();
            self.buf.extend_from_slice(available);
            self.inner.consume(n);
            if self.buf.len() > self.max + 1 {
                self.buf = Vec::new();
                return Err(FrameError::TooLong { max: self.max });
            }
        }
    }

    fn finish(&mut self) -> Result<Vec<u8>, FrameError> {
        let mut line = std::mem::take(&mut self.buf);
        if line.last() == Some(&b'\r') {
            line.pop();
        }
        if line.len() > self.max {
            return Err(FrameError::TooLong { max: self.max });
        }
        Ok(line)
    }
}

/// The preamble's bound, while it is open.
struct Preamble {
    open: bool,
    lines: usize,
    bytes: usize,
}

impl Preamble {
    /// Take one non-JSON line, or `Err` with why it is one too many.
    fn take(&mut self, line: &[u8]) -> Result<String, String> {
        let text = String::from_utf8_lossy(line).into_owned();
        if !self.open {
            return Err(if text.trim().is_empty() {
                "craze wrote a blank line, which is not protocol 1".to_string()
            } else {
                format!("craze wrote a line that is not JSON: {}", clip(&text))
            });
        }
        if self.lines + 1 > PREAMBLE_MAX_LINES || self.bytes + line.len() > PREAMBLE_MAX_BYTES {
            return Err(format!(
                "more than {PREAMBLE_MAX_LINES} lines / {PREAMBLE_MAX_BYTES} bytes of output that is not craze's before its first answer (a login profile printing?): {}",
                clip(&text)
            ));
        }
        self.lines += 1;
        self.bytes += line.len();
        Ok(text)
    }
}

/// One line of something, short enough for an error message.
fn clip(s: &str) -> String {
    const MAX: usize = 200;
    let line = s.lines().next().unwrap_or("").trim();
    let cut = truncate_bytes(line, MAX);
    if cut.len() == line.len() {
        line.to_string()
    } else {
        format!("{cut}…")
    }
}

async fn read_loop<R: AsyncRead + Send + Unpin>(
    reader: R,
    shared: Shared,
    notes: mpsc::Sender<Notification>,
) {
    let mut lines = LineReader::new(reader, OUTBOUND_LINE_MAX);
    let mut preamble = Preamble {
        open: true,
        lines: 0,
        bytes: 0,
    };
    let lost = |why: String| ConnEnd::Lost(why);
    let why = loop {
        let line = match lines.next_line().await {
            Ok(Some(line)) => line,
            Ok(None) => break lost("the connection to craze closed".to_string()),
            Err(e) => break lost(e.to_string()),
        };
        // A blank line is `NotJson` like any other: before the first reply it
        // counts against the preamble (so no stream of them can stall a dial
        // past its bound); after it, it is a protocol fault.
        match wire::parse_line(&line) {
            Ok(Incoming::Reply { id, outcome }) => {
                preamble.open = false;
                let waiter = lock(&shared).pending.remove(&id.to_string());
                if let Some(tx) = waiter {
                    let _ = tx.send(outcome);
                }
            }
            Ok(Incoming::Notification { method, params }) => {
                // Never awaited (the module doc). A consumer that went away is
                // no reason to stop reading: replies still need filing.
                match notes.try_send(Notification { method, params }) {
                    Ok(()) => {
                        if notes.capacity() < NOTIFICATION_QUEUE / 2 {
                            tokio::task::yield_now().await;
                        }
                    }
                    Err(TrySendError::Closed(_)) => {}
                    Err(TrySendError::Full(_)) => break ConnEnd::Backlog,
                }
            }
            Err(LineError::NotJson) => match preamble.take(&line) {
                Ok(text) => lock(&shared).preamble.push(text),
                Err(why) => break lost(why),
            },
            Err(LineError::NotJsonRpc(what)) => {
                break lost(format!(
                    "craze wrote something that is not protocol 1: {what}"
                ))
            }
            Err(LineError::NullId(e)) => {
                break lost(format!(
                    "craze could not read a request of ours ({e}), and cannot say which"
                ))
            }
        }
    };
    lock(&shared).end(why);
}

// ---- the hub hello ----

/// A hub's `hello`, accepted.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct HubHello {
    /// The hub's own id — its roster's `epoch`. A new one is a new hub.
    pub epoch: String,
    pub craze_version: String,
    pub pid: Option<i64>,
    pub capabilities: ConnCapabilities,
}

/// Why a `hello` was not accepted.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum HelloError {
    /// The peer answered, and it cannot be what this client needs: no shared
    /// protocol, an unknown codec, or a hub without the roster or the splice.
    #[error("{0}")]
    TooOld(String),
    /// The peer refused `hello` for another reason, or answered something
    /// that is not a hub's `hello`.
    #[error("{0}")]
    Refused(String),
    /// No answer: the connection ended or the deadline passed first. The
    /// dial classifies this from the process's exit and stderr.
    #[error("{0}")]
    NoAnswer(String),
}

/// How long the first answer may take: a `bridge --hub` that finds no hub
/// starts one first (craze's own recipe allows the same 30 s).
pub const HELLO_DEADLINE: Duration = Duration::from_secs(30);

/// Say `hello` to a hub and judge the answer.
pub async fn hello_hub(
    conn: &Conn,
    client: &ClientInfo,
    deadline: Duration,
) -> Result<HubHello, HelloError> {
    let params = HelloParams::new(client.clone());
    match conn.request(method::HELLO, &params, deadline).await {
        Ok(result) => judge_hub_hello(&result),
        Err(CallError::Refused(e)) => Err(judge_hello_refusal(&e)),
        Err(e) => Err(HelloError::NoAnswer(e.to_string())),
    }
}

/// A refused `hello`: `bad_request`/`protocol_version` is too old (no shared
/// protocol; `data.result.supported` says what the peer speaks); anything else
/// is a refusal (plan 025 §3.3.2).
pub fn judge_hello_refusal(e: &RpcError) -> HelloError {
    let protocol_version = e.data_code.as_deref() == Some(code::BAD_REQUEST)
        && e.reason.as_deref() == Some(reason::PROTOCOL_VERSION);
    if protocol_version {
        let supported = e.result.as_deref().unwrap_or("[]");
        return HelloError::TooOld(format!(
            "craze on this machine shares no protocol with shed (shed speaks [{PROTOCOL}], craze answered {supported}); update craze"
        ));
    }
    HelloError::Refused(format!("craze refused hello: {e}"))
}

/// An answered `hello`: a hub, protocol 1, codecs 1/1, with the roster and the
/// splice.
pub fn judge_hub_hello(result: &Value) -> Result<HubHello, HelloError> {
    let hello = HelloResult::deserialize(result).map_err(|e| {
        HelloError::Refused(format!(
            "craze answered hello with something shed cannot read: {e}"
        ))
    })?;
    if hello.endpoint.kind != "hub" {
        return Err(HelloError::Refused(format!(
            "expected craze's hub to answer, got a {:?} endpoint",
            hello.endpoint.kind
        )));
    }
    check_protocol_and_codecs(&hello)?;
    let caps = hello.capabilities;
    if !caps.roster_subscribe || !caps.connect {
        return Err(HelloError::TooOld(format!(
            "craze's hub on this machine ({}) cannot list or reach sessions (rosterSubscribe {}, connect {}); update craze",
            version_or_unknown(&hello.endpoint.craze_version),
            caps.roster_subscribe,
            caps.connect
        )));
    }
    Ok(HubHello {
        epoch: hello.endpoint.host_id,
        craze_version: hello.endpoint.craze_version,
        pid: hello.endpoint.pid,
        capabilities: caps,
    })
}

/// A host's `hello`, said through the hub's splice, accepted.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct HostHello {
    /// The host's own id — the lane's row id (P11).
    pub host_id: String,
    pub craze_version: String,
}

/// An answered host `hello`: a `host` endpoint, protocol 1, codecs 1/1 — the
/// same rule the hub's is held to (PM "Versioning": a client never folds a
/// codec it does not know). Anything but a host answering is a refusal: the
/// splice landed somewhere it should not have.
pub fn judge_host_hello(result: &Value) -> Result<HostHello, HelloError> {
    let hello = HelloResult::deserialize(result).map_err(|e| {
        HelloError::Refused(format!(
            "craze's host answered hello with something shed cannot read: {e}"
        ))
    })?;
    if hello.endpoint.kind != "host" {
        return Err(HelloError::Refused(format!(
            "expected the session's host to answer through the splice, got a {:?} endpoint",
            hello.endpoint.kind
        )));
    }
    check_protocol_and_codecs(&hello)?;
    Ok(HostHello {
        host_id: hello.endpoint.host_id,
        craze_version: hello.endpoint.craze_version,
    })
}

/// The rule every `hello` — the hub's and the host's — is held to:
/// protocol 1, and the event and snapshot codecs this crate folds. Anything
/// else is `TooOld` and nothing is folded (PM "Versioning").
pub fn check_protocol_and_codecs(hello: &HelloResult) -> Result<(), HelloError> {
    if hello.protocol != PROTOCOL {
        return Err(HelloError::TooOld(format!(
            "craze answered protocol {}, shed speaks {PROTOCOL}; update craze",
            hello.protocol
        )));
    }
    match hello.codecs {
        Some(c) if c.event == CODEC_EVENT && c.snapshot == CODEC_SNAPSHOT => Ok(()),
        other => Err(HelloError::TooOld(format!(
            "craze's codecs are {}, shed folds event {CODEC_EVENT} and snapshot {CODEC_SNAPSHOT}; update shed or craze",
            match other {
                Some(c) => format!("event {} and snapshot {}", c.event, c.snapshot),
                None => "unstated".to_string(),
            }
        ))),
    }
}

fn version_or_unknown(v: &str) -> &str {
    if v.is_empty() {
        "version unknown"
    } else {
        v
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use tokio::io::{duplex, DuplexStream};

    use crate::testing::{full_hub_capabilities, hub_hello_result};

    const SECOND: Duration = Duration::from_secs(5);

    /// A connection over an in-memory duplex: the test holds craze's end.
    fn pair() -> (Conn, Notifications, BufReader<DuplexStream>, DuplexStream) {
        pair_of(64 << 20)
    }

    /// The same, with `cap` bytes of buffer each way.
    fn pair_of(cap: usize) -> (Conn, Notifications, BufReader<DuplexStream>, DuplexStream) {
        let (client_r, server_w) = duplex(cap);
        let (server_r, client_w) = duplex(cap);
        let (conn, notes) = Conn::start(CrazeStream::new(client_r, client_w));
        (conn, notes, BufReader::new(server_r), server_w)
    }

    fn note_line(i: usize) -> String {
        format!("{{\"jsonrpc\":\"2.0\",\"method\":\"event\",\"params\":{{\"seq\":{i}}}}}\n")
    }

    /// A reply's id is matched as the JSON value it is: a numeric `1` does not
    /// answer the request whose id is the string `"1"` — the request waits on
    /// to its deadline, and the next one, answered verbatim, resolves.
    #[tokio::test]
    async fn a_numeric_id_never_answers_a_string_one() {
        let (conn, _notes, mut server, mut w) = pair();
        let first = conn.request("x", &wire::Empty {}, Duration::from_millis(300));
        let answer = async {
            let req = read_request(&mut server).await;
            assert_eq!(req["id"], json!("1"));
            w.write_all(b"{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"wrong\":true}}\n")
                .await
                .unwrap();
        };
        let (got, ()) = tokio::join!(first, answer);
        assert_eq!(got, Err(CallError::Deadline(Duration::from_millis(300))));
        let second = conn.request("x", &wire::Empty {}, SECOND);
        let answer = async {
            read_request(&mut server).await;
            w.write_all(b"{\"jsonrpc\":\"2.0\",\"id\":\"2\",\"result\":{\"right\":true}}\n")
                .await
                .unwrap();
        };
        let (got, ()) = tokio::join!(second, answer);
        assert_eq!(got, Ok(json!({"right": true})));
        assert_ne!(
            json!(1).to_string(),
            json!("1").to_string(),
            "and the other way round"
        );
    }

    /// The reader never waits on its consumer: one notification past the queue
    /// ends the connection as `Backlog` — the reply behind it is not left
    /// waiting on a consumer that is not reading, and the queue held exactly
    /// its bound.
    #[tokio::test]
    async fn a_full_notification_queue_ends_the_connection_and_never_stalls_a_reply() {
        let (conn, mut notes, mut server, mut w) = pair();
        let call = conn.request("x", &wire::Empty {}, SECOND);
        let flood = async {
            read_request(&mut server).await;
            let mut burst = String::new();
            for i in 0..=NOTIFICATION_QUEUE {
                burst.push_str(&note_line(i));
            }
            burst.push_str("{\"jsonrpc\":\"2.0\",\"id\":\"1\",\"result\":{}}\n");
            w.write_all(burst.as_bytes()).await.unwrap();
        };
        let (got, ()) = tokio::join!(call, flood);
        assert!(
            matches!(&got, Err(CallError::Ended(why)) if why.contains("went unread")),
            "{got:?}"
        );
        assert_eq!(conn.ended(), Some(ConnEnd::Backlog));
        let mut held = 0;
        while notes.recv().await.is_some() {
            held += 1;
        }
        assert_eq!(held, NOTIFICATION_QUEUE, "bounded at the queue");
    }

    /// A burst bigger than the queue, already sitting in the reader's buffer,
    /// reaches a consumer that drains as it can — the reader yields its turn
    /// once the queue is half full instead of filing the whole burst first —
    /// and the reply after it resolves.
    #[tokio::test]
    async fn a_burst_bigger_than_the_queue_reaches_a_draining_consumer() {
        let (conn, mut notes, mut server, mut w) = pair();
        let consumer = tokio::spawn(async move {
            let mut n = 0;
            while notes.recv().await.is_some() {
                n += 1;
            }
            n
        });
        let call = conn.request("x", &wire::Empty {}, SECOND);
        let burst = 3 * NOTIFICATION_QUEUE;
        let flood = async {
            read_request(&mut server).await;
            let mut lines = String::new();
            for i in 0..burst {
                lines.push_str(&note_line(i));
            }
            lines.push_str("{\"jsonrpc\":\"2.0\",\"id\":\"1\",\"result\":{}}\n");
            w.write_all(lines.as_bytes()).await.unwrap();
        };
        let (got, ()) = tokio::join!(call, flood);
        assert_eq!(got, Ok(json!({})), "{:?}", conn.ended());
        drop(w);
        drop(conn);
        assert_eq!(consumer.await.unwrap(), burst);
    }

    /// After the first reply a blank or whitespace-only line is a protocol
    /// fault, like any other line that is not JSON.
    #[tokio::test]
    async fn a_blank_line_after_the_first_reply_ends_the_connection() {
        for blank in ["\n", "   \t \n", "\r\n"] {
            let (conn, _notes, mut server, mut w) = pair();
            let first = conn.request("x", &wire::Empty {}, SECOND);
            let answer = async {
                read_request(&mut server).await;
                w.write_all(b"{\"jsonrpc\":\"2.0\",\"id\":\"1\",\"result\":{}}\n")
                    .await
                    .unwrap();
            };
            let (got, ()) = tokio::join!(first, answer);
            assert!(got.is_ok());
            let second = conn.request("x", &wire::Empty {}, SECOND);
            let answer = async {
                read_request(&mut server).await;
                w.write_all(blank.as_bytes()).await.unwrap();
                let _ = w
                    .write_all(b"{\"jsonrpc\":\"2.0\",\"id\":\"2\",\"result\":{}}\n")
                    .await;
            };
            let (got, ()) = tokio::join!(second, answer);
            assert!(
                matches!(&got, Err(CallError::Ended(why)) if why.contains("blank line")),
                "{blank:?}: {got:?}"
            );
        }
    }

    /// A reply without `"jsonrpc":"2.0"` answers nothing: it is a protocol
    /// fault that ends the connection.
    #[tokio::test]
    async fn a_reply_without_jsonrpc_2_0_ends_the_connection() {
        let (conn, _notes, mut server, mut w) = pair();
        let call = conn.request("x", &wire::Empty {}, SECOND);
        let answer = async {
            read_request(&mut server).await;
            w.write_all(b"{\"id\":\"1\",\"result\":{}}\n")
                .await
                .unwrap();
        };
        let (got, ()) = tokio::join!(call, answer);
        assert!(
            matches!(&got, Err(CallError::Ended(why)) if why.contains("not protocol 1")),
            "{got:?}"
        );
    }

    /// The deadline covers the write: a peer that stops reading cannot hold a
    /// request past it, the outcome is unknown, and the connection — which may
    /// now hold half a line — is ended.
    #[tokio::test(start_paused = true)]
    async fn the_deadline_covers_a_write_that_never_finishes() {
        let (conn, _notes, _server, _w) = pair_of(1024);
        let big = json!({"p": "a".repeat(64 << 10)});
        let got = tokio::time::timeout(
            Duration::from_secs(300),
            conn.request("x", &big, Duration::from_secs(120)),
        )
        .await
        .expect("the request ended by its own deadline, not the test's");
        assert_eq!(got, Err(CallError::Deadline(Duration::from_secs(120))));
        assert!(got.unwrap_err().outcome_unknown());
        assert!(
            matches!(conn.ended(), Some(ConnEnd::Lost(why)) if why.contains("did not finish")),
            "{:?}",
            conn.ended()
        );
        assert!(matches!(
            conn.request("x", &wire::Empty {}, SECOND).await,
            Err(CallError::Ended(_))
        ));
    }

    async fn read_request(server: &mut BufReader<DuplexStream>) -> Value {
        let mut line = String::new();
        server.read_line(&mut line).await.unwrap();
        serde_json::from_str(&line).unwrap()
    }

    #[tokio::test]
    async fn replies_are_filed_by_id_whatever_their_order() {
        let (conn, _notes, mut server, mut w) = pair();
        let conn = Arc::new(conn);
        let a = {
            let conn = Arc::clone(&conn);
            tokio::spawn(async move { conn.request("a", &wire::Empty {}, SECOND).await })
        };
        let first = read_request(&mut server).await;
        let b = {
            let conn = Arc::clone(&conn);
            tokio::spawn(async move { conn.request("b", &wire::Empty {}, SECOND).await })
        };
        let second = read_request(&mut server).await;
        assert_eq!(
            (first["id"].clone(), second["id"].clone()),
            (json!("1"), json!("2"))
        );
        // Answered out of order.
        w.write_all(b"{\"jsonrpc\":\"2.0\",\"id\":\"2\",\"result\":{\"m\":\"b\"}}\n{\"jsonrpc\":\"2.0\",\"id\":\"1\",\"result\":{\"m\":\"a\"}}\n").await.unwrap();
        assert_eq!(a.await.unwrap().unwrap(), json!({"m": "a"}));
        assert_eq!(b.await.unwrap().unwrap(), json!({"m": "b"}));
    }

    #[tokio::test]
    async fn notifications_arrive_in_order_and_a_refusal_is_its_own_error() {
        let (conn, mut notes, mut server, mut w) = pair();
        w.write_all(b"{\"jsonrpc\":\"2.0\",\"method\":\"roster\",\"params\":{\"cursor\":1}}\n{\"jsonrpc\":\"2.0\",\"method\":\"reset\",\"params\":{\"reason\":\"omitted\"}}\n").await.unwrap();
        assert_eq!(notes.recv().await.unwrap().method, "roster");
        assert_eq!(notes.recv().await.unwrap().method, "reset");
        let call = conn.request("x", &wire::Empty {}, SECOND);
        let answer = async {
            read_request(&mut server).await;
            w.write_all(b"{\"jsonrpc\":\"2.0\",\"id\":\"1\",\"error\":{\"code\":-32000,\"message\":\"no\",\"data\":{\"code\":\"unavailable\"}}}\n").await.unwrap();
        };
        let (got, ()) = tokio::join!(call, answer);
        assert!(
            matches!(got, Err(CallError::Refused(e)) if e.data_code.as_deref() == Some("unavailable"))
        );
    }

    #[tokio::test]
    async fn an_end_fails_every_waiter_with_why() {
        let (conn, mut notes, mut server, w) = pair();
        let call = conn.request("x", &wire::Empty {}, SECOND);
        let end = async {
            read_request(&mut server).await;
            drop(w);
        };
        let (got, ()) = tokio::join!(call, end);
        assert!(matches!(&got, Err(CallError::Ended(_))), "{got:?}");
        assert!(got.unwrap_err().outcome_unknown());
        assert!(
            notes.recv().await.is_none(),
            "the notifications end with it"
        );
        assert_eq!(
            conn.ended(),
            Some(ConnEnd::Lost("the connection to craze closed".to_string()))
        );
    }

    #[tokio::test(start_paused = true)]
    async fn a_reply_that_never_comes_is_a_deadline_and_an_unknown_outcome() {
        let (conn, _notes, _server, _w) = pair();
        let got = conn
            .request("x", &wire::Empty {}, Duration::from_secs(30))
            .await;
        assert_eq!(got, Err(CallError::Deadline(Duration::from_secs(30))));
        assert!(got.unwrap_err().outcome_unknown());
    }

    /// A request line whose content is exactly `len` bytes.
    fn padded(len: usize) -> Value {
        let base = wire::request_line("1", "x", &json!({"p": ""}))
            .unwrap()
            .len();
        json!({"p": "a".repeat(len - base)})
    }

    /// The write limit, from the client's side: exactly 4 MiB is written, a
    /// byte more is refused before anything reaches the wire.
    #[tokio::test]
    async fn the_client_never_writes_a_line_over_4_mib() {
        let (conn, _notes, mut server, mut w) = pair();
        let over = conn
            .request("x", &padded(INBOUND_LINE_MAX + 1), SECOND)
            .await;
        assert_eq!(
            over,
            Err(CallError::TooLong {
                bytes: INBOUND_LINE_MAX + 1
            })
        );
        let at_limit = padded(INBOUND_LINE_MAX);
        let at = conn.request("x", &at_limit, SECOND);
        let answer = async {
            let mut line = String::new();
            server.read_line(&mut line).await.unwrap();
            // The refused request never reached the wire: this is the next
            // id's line, exactly the limit long.
            assert_eq!(line.len(), INBOUND_LINE_MAX + 1, "content + the newline");
            assert!(line.starts_with(r#"{"jsonrpc":"2.0","id":"2""#));
            w.write_all(b"{\"jsonrpc\":\"2.0\",\"id\":\"2\",\"result\":{}}\n")
                .await
                .unwrap();
        };
        let (got, ()) = tokio::join!(at, answer);
        assert_eq!(got, Ok(json!({})));
    }

    /// A reply line of exactly `len` bytes of content.
    fn reply_of(len: usize) -> Vec<u8> {
        let base = br#"{"jsonrpc":"2.0","id":"1","result":{"p":""}}"#.len();
        let mut line = format!(
            r#"{{"jsonrpc":"2.0","id":"1","result":{{"p":"{}"}}}}"#,
            "a".repeat(len - base)
        )
        .into_bytes();
        assert_eq!(line.len(), len);
        line.push(b'\n');
        line
    }

    /// The read limit: a 16 MiB reply is read, a byte more ends the
    /// connection as a protocol fault.
    #[tokio::test]
    async fn the_client_reads_a_16_mib_line_and_not_a_byte_more() {
        for (len, ok) in [(OUTBOUND_LINE_MAX, true), (OUTBOUND_LINE_MAX + 1, false)] {
            let (conn, _notes, mut server, mut w) = pair();
            let call = conn.request("x", &wire::Empty {}, Duration::from_secs(60));
            let answer = async {
                read_request(&mut server).await;
                // Craze's end may close with the write blocked: ignore it.
                let _ = w.write_all(&reply_of(len)).await;
                w
            };
            let (got, _w) = tokio::join!(call, answer);
            if ok {
                assert!(got.is_ok(), "a {len}-byte line is read: {got:?}");
            } else {
                assert!(
                    matches!(&got, Err(CallError::Ended(why)) if why.contains("over")),
                    "{got:?}"
                );
            }
        }
    }

    /// A `\r` before the `\n` is tolerated and does not count.
    #[tokio::test]
    async fn a_crlf_line_is_one_line() {
        let mut r = LineReader::new(&b"abc\r\ndef"[..], 3);
        assert_eq!(r.next_line().await.unwrap().unwrap(), b"abc");
        assert_eq!(
            r.next_line().await.unwrap().unwrap(),
            b"def",
            "an unterminated last line is a line"
        );
        assert!(r.next_line().await.unwrap().is_none());
        let mut r = LineReader::new(&b"abcd\n"[..], 3);
        assert!(matches!(
            r.next_line().await,
            Err(FrameError::TooLong { max: 3 })
        ));
    }

    /// Up to 16 non-JSON lines are skipped before the first reply, and kept.
    #[tokio::test]
    async fn sixteen_preamble_lines_are_skipped_and_kept() {
        let (conn, _notes, mut server, mut w) = pair();
        let call = conn.request("x", &wire::Empty {}, SECOND);
        let answer = async {
            read_request(&mut server).await;
            for i in 0..PREAMBLE_MAX_LINES {
                w.write_all(format!("motd line {i}\n").as_bytes())
                    .await
                    .unwrap();
            }
            w.write_all(b"{\"jsonrpc\":\"2.0\",\"id\":\"1\",\"result\":{}}\n")
                .await
                .unwrap();
        };
        let (got, ()) = tokio::join!(call, answer);
        assert_eq!(got, Ok(json!({})));
        assert_eq!(conn.preamble().len(), PREAMBLE_MAX_LINES);
        assert_eq!(conn.preamble()[0], "motd line 0");
    }

    /// The seventeenth line is refused, and so is a preamble over 4 KiB — and
    /// the refusal says what was printed.
    #[tokio::test]
    async fn the_seventeenth_line_or_a_4_kib_overflow_is_refused() {
        let cases: [Vec<String>; 3] = [
            (0..=PREAMBLE_MAX_LINES)
                .map(|i| format!("line {i}"))
                .collect(),
            vec!["a".repeat(2048), "b".repeat(2048), "c".to_string()],
            // Exactly 4096 is within the bound; this case stays green below.
            vec!["a".repeat(2048), "b".repeat(2048)],
        ];
        for (n, lines) in cases.iter().enumerate() {
            let (conn, _notes, mut server, mut w) = pair();
            let call = conn.request("x", &wire::Empty {}, SECOND);
            let answer = async {
                read_request(&mut server).await;
                for l in lines {
                    w.write_all(format!("{l}\n").as_bytes()).await.unwrap();
                }
                let _ = w
                    .write_all(b"{\"jsonrpc\":\"2.0\",\"id\":\"1\",\"result\":{}}\n")
                    .await;
                w
            };
            let (got, _w) = tokio::join!(call, answer);
            if n == 2 {
                assert_eq!(
                    got,
                    Ok(json!({})),
                    "exactly {PREAMBLE_MAX_BYTES} bytes is within the bound"
                );
            } else {
                assert!(
                    matches!(&got, Err(CallError::Ended(why)) if why.contains("before its first answer")),
                    "case {n}: {got:?}"
                );
            }
        }
    }

    /// After the first reply, a non-JSON line is a protocol fault.
    #[tokio::test]
    async fn a_non_json_line_after_the_first_reply_ends_the_connection() {
        let (conn, _notes, mut server, mut w) = pair();
        let first = conn.request("x", &wire::Empty {}, SECOND);
        let answer = async {
            read_request(&mut server).await;
            w.write_all(b"{\"jsonrpc\":\"2.0\",\"id\":\"1\",\"result\":{}}\n")
                .await
                .unwrap();
        };
        let (got, ()) = tokio::join!(first, answer);
        assert!(got.is_ok());
        let second = conn.request("x", &wire::Empty {}, SECOND);
        let answer = async {
            read_request(&mut server).await;
            w.write_all(b"bash: warning: something\n").await.unwrap();
        };
        let (got, ()) = tokio::join!(second, answer);
        assert!(
            matches!(&got, Err(CallError::Ended(why)) if why.contains("not JSON")),
            "{got:?}"
        );
    }

    /// A hub `hello` result (the scripted hub's own) with these codecs.
    fn hub_hello(caps: Value, codecs: Value) -> Value {
        let mut v = hub_hello_result("0a1b2c3d4e5f", caps);
        v["codecs"] = codecs;
        v
    }

    #[test]
    fn a_hub_hello_needs_codecs_one_and_the_roster_and_the_splice() {
        let ok = judge_hub_hello(&hub_hello(
            full_hub_capabilities(),
            json!({"event": 1, "snapshot": 1}),
        ))
        .unwrap();
        assert_eq!(ok.epoch, "0a1b2c3d4e5f");
        assert!(ok.capabilities.session_create && ok.capabilities.create_options);
        for codecs in [
            json!({"event": 2, "snapshot": 1}),
            json!({"event": 1, "snapshot": 2}),
            Value::Null,
        ] {
            assert!(
                matches!(
                    judge_hub_hello(&hub_hello(full_hub_capabilities(), codecs.clone())),
                    Err(HelloError::TooOld(_))
                ),
                "codecs {codecs} are not foldable"
            );
        }
        for missing in ["rosterSubscribe", "connect"] {
            let mut caps = full_hub_capabilities();
            caps[missing] = json!(false);
            assert!(matches!(
                judge_hub_hello(&hub_hello(caps, json!({"event": 1, "snapshot": 1}))),
                Err(HelloError::TooOld(_))
            ));
        }
        let mut host = hub_hello(full_hub_capabilities(), json!({"event": 1, "snapshot": 1}));
        host["endpoint"]["kind"] = json!("host");
        assert!(matches!(
            judge_hub_hello(&host),
            Err(HelloError::Refused(_))
        ));
    }

    /// A host's `hello` through the splice: a host endpoint, protocol 1,
    /// codecs 1/1 — anything else refused or too old.
    #[test]
    fn a_host_hello_is_a_host_at_protocol_one_and_codecs_one() {
        let ok = crate::testing::host_hello_result("0123456789ab");
        assert_eq!(judge_host_hello(&ok).unwrap().host_id, "0123456789ab");
        let mut hub = ok.clone();
        hub["endpoint"]["kind"] = json!("hub");
        assert!(matches!(
            judge_host_hello(&hub),
            Err(HelloError::Refused(_))
        ));
        let mut codec = ok.clone();
        codec["codecs"]["snapshot"] = json!(2);
        assert!(matches!(
            judge_host_hello(&codec),
            Err(HelloError::TooOld(_))
        ));
        let mut proto = ok;
        proto["protocol"] = json!(2);
        assert!(matches!(
            judge_host_hello(&proto),
            Err(HelloError::TooOld(_))
        ));
    }

    /// `close` ends a live connection on purpose: a waiter fails with the
    /// reason, the notifications end, and a later request is refused at once.
    #[tokio::test]
    async fn close_ends_the_connection_from_outside() {
        let (conn, mut notes, mut server, _w) = pair();
        let conn = Arc::new(conn);
        let c = Arc::clone(&conn);
        let call = tokio::spawn(async move { c.request("x", &wire::Empty {}, SECOND).await });
        read_request(&mut server).await;
        conn.close("a verb's deadline passed");
        let got = call.await.unwrap();
        assert!(
            matches!(&got, Err(CallError::Ended(why)) if why.contains("deadline passed")),
            "{got:?}"
        );
        let ended = tokio::time::timeout(SECOND, notes.recv())
            .await
            .expect("the notifications END — not hang on a reader nobody stopped");
        assert!(ended.is_none(), "the notifications end");
        assert_eq!(
            conn.ended(),
            Some(ConnEnd::Lost("a verb's deadline passed".into()))
        );
        assert!(matches!(
            conn.request("y", &wire::Empty {}, SECOND).await,
            Err(CallError::Ended(_))
        ));
    }

    #[test]
    fn a_protocol_version_refusal_is_too_old_and_any_other_is_a_refusal() {
        let pv = RpcError::from_value(&json!({"code": -32000, "message": "no protocol in common",
            "data": {"code": "bad_request", "reason": "protocol_version", "result": {"supported": [2]}}}));
        assert!(matches!(judge_hello_refusal(&pv), HelloError::TooOld(m) if m.contains("[2]")));
        let closing =
            RpcError::from_value(&json!({"code": -32000, "message": "the hub is closing",
            "data": {"code": "unavailable", "reason": "closing"}}));
        assert!(matches!(
            judge_hello_refusal(&closing),
            HelloError::Refused(_)
        ));
        let other_bad = RpcError::from_value(&json!({"code": -32602, "message": "x",
            "data": {"code": "bad_request", "reason": "bad_request"}}));
        assert!(matches!(
            judge_hello_refusal(&other_bad),
            HelloError::Refused(_)
        ));
    }
}
