//! The opencode SSE/REST transport + verb lane — a port of
//! `internal/ext/rc/watch_opencode_transport.go`.
//!
//! [`OpencodeWatcher`] is the SSE/REST-backed [`SessionWatcher`] for an
//! opencode session. Unlike the codex/claude file watchers — which tail a
//! durable append-only JSONL file — opencode is client/server: the bare TUI
//! runs an embedded HTTP+SSE server on a per-session port
//! (`SHED_RC_OPENCODE_PORT`, stamped at create), and this watcher subscribes
//! to that server's `/event` stream (plus a REST seed) as a SECOND client.
//!
//! CONCURRENCY MODEL (the load-bearing invariant, ported from
//! `watch_opencode_transport.go:32-55`):
//!
//! - A single background THREAD ([`OpencodeWatcher::run`]) owns all READ-side
//!   HTTP I/O: correlation, the SSE read loop, and the REST seed. It NEVER
//!   mutates the fold. Under the watcher mutex it only (a) pushes
//!   routing-filtered raw envelope payloads (and marker records) onto the
//!   bounded inbox, (b) updates the transport-health fields
//!   (connected/last_frame_at), and (c) sets the discovered-confirmed-id
//!   slot. It NEVER holds the lock across a network wait.
//! - `refresh(now)` — called by reconcile on the MAIN thread — is the ONLY
//!   place the STREAM mutates the fold: under the mutex it drains the inbox →
//!   `fold.apply_line`, handles the seed-complete / overflow-gap markers, and
//!   reads the fold's verdict + feed.
//! - The ONE exception, and the only fold state written off the reconcile
//!   thread, is the APPROVALS map: [`OpencodeWatcher::mark_approval_resolved`]
//!   records a resolution from the verb-handler thread the moment the
//!   approvals verb's upstream POST succeeds (it must be synchronous — a
//!   same-decision replay arriving before the next tick would otherwise
//!   re-POST), and `approval_state`/`pending_approvals` read it from handler
//!   threads. Every one of those takes the SAME watcher mutex, so the fold
//!   still has exactly one writer at a time; what varies is only WHICH thread
//!   may be that writer.
//!
//! HTTP CLIENT (deliberate divergence from the plan's ureq lean, per its own
//! sanctioned fallback — the callout): the transport thread speaks a
//! HAND-ROLLED loopback HTTP/1.1 client over `std::net::TcpStream` rather
//! than ureq, because close() must UNBLOCK an in-flight body read (Go closes
//! the response body for exactly this reason — cancelling a context alone
//! does not unblock `Body.Read`), and only an owned socket gives
//! `shutdown()`-from-another-thread semantics. The client is loopback-only,
//! proxy-free by construction, sends `Connection: close` (per-request TCP —
//! fine on loopback), and decodes content-length / chunked / EOF-delimited
//! bodies (real opencode streams chunked SSE). The VERB lane's three POSTs
//! use the async `reqwest` client the broker already carries — they run on
//! handler tasks, are strictly bounded (5s), and share no socket with the
//! read side.

use std::collections::HashMap;
use std::io::{Read, Write};
use std::net::TcpStream;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Condvar, Mutex};
use std::time::Duration;

use chrono::{DateTime, Utc};
use serde::Deserialize;
use serde_json::value::RawValue;
use shed_core::rc::RcActivity;
use shed_core::rc_agents::ENV_AGENT_SESSION;

use super::messages::{null_default, FeedApproval, FeedMessage};
use super::watch::ActivityFold;
use super::watch::{
    json_first_byte, object_opt, raw_opt, watcher_freshness, ConfirmedAgentIdDrainer, LogFn,
    MessageProducer, SessionWatcher,
};
use super::watch_opencode::{OpencodeFold, OC_APPROVAL_SEED_TYPE};

// ---------------------------------------------------------------------------
// bounds + tuning (watch_opencode_transport.go:171-201)
// ---------------------------------------------------------------------------

/// Inbox bounds by BOTH element count AND total bytes
/// (`maxInboxItems`/`maxInboxBytes`). Overflow drops the item and forces a
/// full reconnect+reseed (drop-oldest+note_gap alone can permanently miss
/// permission.replied / idle / tool-completion).
pub(crate) const MAX_INBOX_ITEMS: usize = 1024;
pub(crate) const MAX_INBOX_BYTES: usize = 4 << 20;

/// One SSE line (one `data:` field) / one accumulated frame
/// (`maxSSELineBytes`/`maxSSEFrameBytes`). Oversized → the read errors →
/// reconnect.
pub(crate) const MAX_SSE_LINE_BYTES: usize = 1 << 20;
pub(crate) const MAX_SSE_FRAME_BYTES: usize = 4 << 20;

/// A single REST response body cap (`maxRESTBytes`; the SSE GET is
/// deliberately NOT capped — it streams).
const MAX_REST_BYTES: usize = 8 << 20;

/// One response HEAD's total size. Go gets this from `http.ReadResponse`
/// (`DefaultMaxHeaderBytes`); the hand-rolled client has to bound it itself,
/// or a peer that streams headers forever keeps the parse loop alive (the
/// per-read deadline resets on every line).
const MAX_HEAD_BYTES: usize = 64 * 1024;

/// Connect / response-header / REST-call bounds
/// (`dialTimeout`/`headerTimeout`/`restTimeout`). NONE of these is an overall
/// timeout on the SSE GET.
const DIAL_TIMEOUT: Duration = Duration::from_secs(3);
const HEADER_TIMEOUT: Duration = Duration::from_secs(5);
const REST_TIMEOUT: Duration = Duration::from_secs(5);

/// The reconnect backoff floor/cap (`ocBackoffBase`/`ocBackoffMax`; jittered).
pub(crate) const OC_BACKOFF_BASE: Duration = Duration::from_millis(100);
pub(crate) const OC_BACKOFF_MAX: Duration = Duration::from_secs(5);

/// How long a settled/working verdict stays authoritative after the last SSE
/// frame (`ocFrameStaleWindow`). opencode heartbeats ~every 10s, so ~3 missed
/// heartbeats means the stream is wedged even if the socket has not errored.
pub(crate) const OC_FRAME_STALE_WINDOW: Duration = Duration::from_secs(30);

/// One verb's upstream bound (`ocVerbTimeout`) — the same 5s as the REST
/// seeds: these are loopback POSTs to a process on this machine, and a client
/// waiting on a steer must get an answer (or a retryable 409) promptly.
const OC_VERB_TIMEOUT: Duration = Duration::from_secs(5);

// ---------------------------------------------------------------------------
// errors (watch_opencode_transport.go:203-208, 733)
// ---------------------------------------------------------------------------

/// The transport's error vocabulary. The named variants are the sentinels the
/// Go code compares with `errors.Is`; everything else rides `Other`.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum OcWatcherError {
    #[error("opencode watcher closed")]
    Closed,
    #[error("opencode watcher inbox overflow")]
    InboxOverflow,
    #[error("opencode watcher sse frame too large")]
    SseOversize,
    #[error("opencode watcher status seed failed")]
    StatusSeedFailed,
    /// The unpinned-session sentinel (`errNoAgentSession`): the watcher is
    /// healthy but no opencode session has been correlated yet. Its text is
    /// operator-facing — the handler surfaces it verbatim.
    #[error(
        "agent session not established yet — deliver the first prompt via the prompt/attach path"
    )]
    NoAgentSession,
    /// An upstream non-2xx (`ocStatusError`): the status is safe to report to
    /// a caller; the path is hub-log-only (it embeds the pinned opencode
    /// session id, which the wire contract never discloses).
    #[error("POST {path}: status {status}")]
    Status { status: u16, path: String },
    #[error("{0}")]
    Other(String),
}

impl OcWatcherError {
    fn other(err: impl std::fmt::Display) -> OcWatcherError {
        OcWatcherError::Other(err.to_string())
    }
}

// ---------------------------------------------------------------------------
// inbox records (watch_opencode_transport.go:147-169)
// ---------------------------------------------------------------------------

/// The records the I/O thread pushes onto the inbox (`inboxKind`/`inboxItem`).
enum InboxItem {
    /// A raw `{id,type,properties}` envelope → `fold.apply_line`.
    Payload(Vec<u8>),
    /// The seed + buffered replay is fully applied → authoritative. Tagged
    /// with the connection generation live when enqueued; refresh honors it
    /// only while that generation is still current (fix #2).
    SeedComplete { gen: u64, fallback: SeedFallback },
    /// The inbox overflowed → `fold.note_gap()` + forced resync (gen-tagged
    /// for the same reason).
    OverflowGap { gen: u64 },
}

/// The REST `/session/status` result captured during a seed (`seedFallback`).
/// Applied by refresh AFTER the seed's payloads (and any buffered live events
/// in the same batch) as a FALLBACK — only when no live
/// session.status/session.idle boundary was folded (§3.4).
#[derive(Debug, Clone, Copy, Default)]
pub(crate) struct SeedFallback {
    /// A status seed result is present.
    set: bool,
    /// true → the session was idle at connect; false → busy.
    idle: bool,
}

// ---------------------------------------------------------------------------
// the watcher
// ---------------------------------------------------------------------------

/// The SSE/REST-backed opencode watcher (`opencodeWatcher`,
/// `watch_opencode_transport.go:59`).
pub struct OpencodeWatcher {
    /// `http://127.0.0.1:<port>` — kept as host/port for the raw client.
    port: u16,
    /// Canonical (symlink-resolved) session workdir, for the dir-match pin.
    workdir: String,
    /// A prior back-written SHED_RC_AGENT_SESSION ("" if none) — a trusted pin.
    prior_id: String,
    now_fn: Arc<dyn Fn() -> DateTime<Utc> + Send + Sync>,
    logf: LogFn,
    /// The verb lane's async client (loopback POSTs; no proxy, short timeout).
    verb_client: reqwest::Client,

    /// The state Go guards with `w.mu`.
    state: Mutex<WatcherState>,
    /// Fast-path closed mirror (authoritative `closed` lives in `state`; this
    /// is set AFTER it under the same close call and is what the sleep/backoff
    /// wait and the I/O loop's cheap checks read).
    closed_flag: AtomicBool,
    /// Wakes the backoff sleep on close (Go selects on ctx.Done()).
    wake_mu: Mutex<()>,
    wake: Condvar,
    /// Set when run() exits; the test/shutdown join point (Go's `done` chan).
    done_pair: Arc<(Mutex<bool>, Condvar)>,
}

struct WatcherState {
    fold: OpencodeFold,

    /// Terminal: refresh/snapshot no-op, run exits.
    closed: bool,
    /// The in-flight SSE stream socket, for close() to unblock a blocked
    /// read (Go's `body io.Closer`; a socket clone, shut down on close).
    body: Option<TcpStream>,
    /// The in-flight REST socket (Go bounds REST calls with a ctx derived
    /// from w.ctx — close() cancelling it unblocks a blocked Do; here the
    /// socket shutdown is that cancellation). SEPARATE from `body`: a REST
    /// seed runs while the SSE stream stays open, and sharing one slot would
    /// deregister the stream socket mid-connection (found by the
    /// close-during-blocked-read mirror).
    rest_body: Option<TcpStream>,

    /// The connection-generation counter (`gen`): incremented on each connect
    /// attempt; markers from a superseded generation are ignored.
    gen: u64,

    /// The current reconnect backoff (run writes; tests read).
    backoff: Duration,

    /// The inbox: the I/O thread pushes; refresh drains. Bounded by BOTH
    /// count and bytes.
    inbox: Vec<InboxItem>,
    inbox_bytes: usize,

    /// Approval ids whose upstream resolve POST is IN FLIGHT (id → decision) —
    /// the verb path's atomic claim.
    resolving: HashMap<String, String>,

    // correlation
    /// The session id we filter/seed on ("" while still searching).
    pinned_id: String,
    /// An SSE-discovered pin awaiting drain ("" = none/drained).
    confirmed_id: String,
    /// A discovered pin was enqueued once (do not re-enqueue on reconnect).
    confirmed_once: bool,

    // transport health (I/O thread writes; snapshot reads)
    connected: bool,
    /// Stamp of the most recent SSE frame (heartbeats count).
    last_frame_at: Option<DateTime<Utc>>,
    /// A seedComplete marker has been folded on the CURRENT connection.
    seed_applied: bool,

    // fold-derived verdict (refresh writes; snapshot reads)
    last_event_at: Option<DateTime<Utc>>,
    cur_activity: RcActivity,
    cur_message: String,
    cur_settled: bool,
    pending: Vec<FeedMessage>,
}

impl WatcherState {
    /// Records an inbox overflow (`enqueueOverflowLocked`). It revokes
    /// authority IMMEDIATELY under the same lock (seed_applied=false, fix #7)
    /// so snapshot is non-authoritative before the run thread even observes
    /// the forced reconnect. The overflow-gap marker (tagged with the current
    /// generation, coalesced) drives note_gap in refresh; markers carry no
    /// bytes and bypass the byte bound (tiny and bounded in number).
    fn enqueue_overflow_locked(&mut self) {
        self.seed_applied = false;
        if matches!(self.inbox.last(), Some(InboxItem::OverflowGap { .. })) {
            return; // coalesce consecutive overflow markers
        }
        let gen = self.gen;
        self.inbox.push(InboxItem::OverflowGap { gen });
    }
}

/// The safe-path-segment SHAPE an opencode session id must have to be usable
/// as a pin (`ocSessionIDRe` — `^[A-Za-z0-9_-]+$`, ≤256).
///
/// INVARIANT HARDENING, not a trust boundary: what this protects is the WS-B
/// property that a request arriving over the server proxy can never be made
/// to address another opencode session. Rejected pins are treated as NO pin.
pub(crate) fn valid_opencode_session_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= 256
        && id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-')
}

/// Builds a session-scoped route with EVERY interpolated segment escaped
/// (`ocSessionPath`). The pin is already shape-validated — the escaping is
/// the second layer, and it is the ONLY guard on the approval id, which
/// reaches here from the request path (the handler's ApprovalIDRe admits dots
/// and colons, so escaping, not the grammar, is what keeps a segment a
/// segment).
pub(crate) fn oc_session_path(id: &str, tail: &[&str]) -> String {
    let mut p = format!("/session/{}", path_escape(id));
    for seg in tail {
        p.push('/');
        p.push_str(&path_escape(seg));
    }
    p
}

/// Go's `url.PathEscape`: percent-encode everything outside its
/// encodePathSegment keep-set — alphanumerics, the unreserved marks `-._~`,
/// and `$&+:=@` (`/` `;` `,` and the RFC-2396 marks `!'()*` ARE escaped —
/// verified byte-for-byte over 0-255 against the Go oracle; H8 review).
fn path_escape(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for b in s.bytes() {
        let keep = b.is_ascii_alphanumeric()
            || matches!(
                b,
                b'-' | b'.' | b'_' | b'~' | b'$' | b'&' | b'+' | b'=' | b':' | b'@'
            );
        if keep {
            out.push(b as char);
        } else {
            out.push_str(&format!("%{b:02X}"));
        }
    }
    out
}

impl OpencodeWatcher {
    /// Builds a watcher for an opencode session's embedded server on
    /// `127.0.0.1:<port>` and starts its background I/O thread
    /// (`newOpencodeWatcher`). NON-BLOCKING: the constructor returns
    /// immediately and correlation/seed/subscribe happen on the thread (a
    /// fresh idle TUI has no session yet, so the thread must not block the
    /// caller waiting for one). `agent_id` is a prior back-written
    /// SHED_RC_AGENT_SESSION ("" if none): when set it is the trusted pin and
    /// no SSE discovery is needed. The watcher is runner-free — it does no
    /// tmux I/O; the discovered id is surfaced via the confirmed-id drain for
    /// reconcile to back-write.
    pub fn new(
        port: u16,
        workdir: &str,
        agent_id: &str,
        now_fn: Arc<dyn Fn() -> DateTime<Utc> + Send + Sync>,
        logf: Option<LogFn>,
    ) -> Arc<OpencodeWatcher> {
        let logf = logf.unwrap_or_else(super::watch::noop_logf);
        // A prior back-write that is not a safe path segment is DISCARDED
        // (not merely unused): treating it as no pin at all keeps the
        // addressing invariant intact and lets correlation discover — and
        // back-write — a real id.
        let mut agent_id = agent_id.to_string();
        if !agent_id.is_empty() && !valid_opencode_session_id(&agent_id) {
            logf(&format!(
                "rc hub: opencode watcher on port {port} ignoring a malformed {ENV_AGENT_SESSION} pin"
            ));
            agent_id.clear();
        }
        let w = Arc::new(OpencodeWatcher {
            port,
            workdir: canonical_dir(workdir),
            prior_id: agent_id.clone(),
            now_fn,
            logf,
            verb_client: reqwest::Client::builder()
                .no_proxy() // loopback only — env proxies disabled, like Go's nil-Proxy transport
                .connect_timeout(DIAL_TIMEOUT)
                .build()
                .expect("loopback reqwest client builds"),
            state: Mutex::new(WatcherState {
                fold: OpencodeFold::new(),
                closed: false,
                body: None,
                rest_body: None,
                gen: 0,
                backoff: OC_BACKOFF_BASE,
                inbox: Vec::new(),
                inbox_bytes: 0,
                resolving: HashMap::new(),
                pinned_id: agent_id, // a prior back-write is the pin; "" = "search the SSE stream"
                confirmed_id: String::new(),
                confirmed_once: false,
                connected: false,
                last_frame_at: None,
                seed_applied: false,
                last_event_at: None,
                cur_activity: RcActivity::Unknown,
                cur_message: String::new(),
                cur_settled: false,
                pending: Vec::new(),
            }),
            closed_flag: AtomicBool::new(false),
            wake_mu: Mutex::new(()),
            wake: Condvar::new(),
            done_pair: Arc::new((Mutex::new(false), Condvar::new())),
        });
        let io = Arc::clone(&w);
        std::thread::Builder::new()
            .name("rc-hub-opencode".into())
            .spawn(move || {
                io.run();
                let (done, cv) = &*io.done_pair;
                *done
                    .lock()
                    .unwrap_or_else(std::sync::PoisonError::into_inner) = true;
                cv.notify_all();
            })
            .expect("spawn opencode watcher thread");
        w
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, WatcherState> {
        self.state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// Waits for the I/O thread to exit (the Go `done` channel's test/shutdown
    /// join point). Returns false on timeout.
    pub fn wait_done(&self, timeout: Duration) -> bool {
        let (done, cv) = &*self.done_pair;
        let guard = done
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let (guard, _res) = cv
            .wait_timeout_while(guard, timeout, |d| !*d)
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        *guard
    }

    fn is_closed(&self) -> bool {
        self.closed_flag.load(Ordering::Relaxed)
    }

    // ---- inbox / health / body plumbing (all under the mutex, never across
    // a network wait) ----

    /// Enqueues a raw envelope (`pushPayload`). On overflow (either bound) it
    /// enqueues a single overflow-gap marker instead and returns false — the
    /// caller must then force a reconnect+reseed. A closed watcher drops
    /// silently.
    fn push_payload(&self, payload: Vec<u8>) -> bool {
        let mut w = self.lock();
        if w.closed {
            return false;
        }
        if w.inbox.len() >= MAX_INBOX_ITEMS || w.inbox_bytes + payload.len() > MAX_INBOX_BYTES {
            w.enqueue_overflow_locked();
            return false;
        }
        w.inbox_bytes += payload.len();
        w.inbox.push(InboxItem::Payload(payload));
        true
    }

    /// Enqueues the seed-complete barrier tagged with the current generation
    /// (`pushSeedComplete`). A closed watcher drops it.
    fn push_seed_complete(&self, fallback: SeedFallback) {
        let mut w = self.lock();
        if w.closed {
            return;
        }
        let gen = w.gen;
        w.inbox.push(InboxItem::SeedComplete { gen, fallback });
    }

    /// Advances the connection-generation counter at the start of each
    /// connect attempt (`beginGeneration`): any marker still queued from the
    /// prior generation is thereby invalidated.
    fn begin_generation(&self) {
        self.lock().gen += 1;
    }

    fn set_backoff(&self, d: Duration) {
        self.lock().backoff = d;
    }

    /// Test-visible backoff read (`getBackoff`).
    #[cfg(test)]
    pub(crate) fn get_backoff(&self) -> Duration {
        self.lock().backoff
    }

    /// Records the SSE stream as up/down (`setConnected`). On disconnect it
    /// also resets seed_applied so the watcher is non-authoritative until its
    /// NEXT re-seed marker passes.
    fn set_connected(&self, up: bool) {
        let mut w = self.lock();
        w.connected = up;
        if !up {
            w.seed_applied = false;
        }
    }

    /// Stamps the last-frame time (`markFrame`; heartbeats count toward
    /// transport freshness).
    fn mark_frame(&self) {
        let now = (self.now_fn)();
        self.lock().last_frame_at = Some(now);
    }

    /// Records the correlated session id (`setPinned`). When discovered
    /// (SSE-trusted, not a prior back-write) its id is enqueued ONCE for the
    /// confirmed-id drain. A pin that is not a safe path segment is refused
    /// outright.
    fn set_pinned(&self, id: &str, discovered: bool) {
        if !id.is_empty() && !valid_opencode_session_id(id) {
            (self.logf)(&format!(
                "rc hub: opencode watcher :{} ignoring malformed session id",
                self.port
            ));
            return;
        }
        let mut w = self.lock();
        w.pinned_id = id.to_string();
        if discovered && !w.confirmed_once && !id.is_empty() && id != self.prior_id {
            w.confirmed_id = id.to_string();
            w.confirmed_once = true;
        }
    }

    /// Test-visible pin read (`getPinned`).
    pub(crate) fn get_pinned(&self) -> String {
        self.lock().pinned_id.clone()
    }

    /// Registers an in-flight I/O socket so close() can unblock a read
    /// (`registerBody`; `streaming` picks the SSE vs REST slot). Returns
    /// false when already closed (the caller shuts the socket down
    /// immediately — no post-close body registered).
    fn register_body(&self, sock: &TcpStream, streaming: bool) -> bool {
        let mut w = self.lock();
        if w.closed {
            return false;
        }
        let clone = sock.try_clone().ok();
        if streaming {
            w.body = clone;
        } else {
            w.rest_body = clone;
        }
        true
    }

    /// Drops a registered socket (`clearBody`) and shuts it down.
    fn clear_body(&self, streaming: bool) {
        let sock = {
            let mut w = self.lock();
            if streaming {
                w.body.take()
            } else {
                w.rest_body.take()
            }
        };
        if let Some(sock) = sock {
            let _ = sock.shutdown(std::net::Shutdown::Both);
        }
    }

    #[cfg(test)]
    pub(crate) fn last_frame_at(&self) -> Option<DateTime<Utc>> {
        self.lock().last_frame_at
    }

    #[cfg(test)]
    pub(crate) fn fold_apply_for_test(&self, line: &[u8]) {
        let mut w = self.lock();
        w.fold.apply_line(line);
    }
}

/// Resolves symlinks (falling back to a lexical clean) so both sides of the
/// directory match are compared canonically (`canonicalDir`).
///
/// Go's `filepath.EvalSymlinks` preserves relativity — a relative input
/// yields a relative result — while `fs::canonicalize` always absolutizes
/// against the CWD. Left as-is, a RELATIVE event directory that happens to
/// resolve under the hub's CWD to the session's workdir would pin in Rust
/// where Go ignores it (H8 review LOW). A relative input is therefore
/// re-relativized against the CWD; one that resolved OUTSIDE the CWD stays
/// absolute (an approximation — Go may render that as `..`-relative — but
/// the pin comparison is against an absolute workdir either way).
pub(crate) fn canonical_dir(p: &str) -> String {
    if p.is_empty() {
        return String::new();
    }
    if let Ok(real) = std::fs::canonicalize(p) {
        let real = if std::path::Path::new(p).is_relative() {
            std::env::current_dir()
                .ok()
                .and_then(|cwd| real.strip_prefix(&cwd).map(|r| r.to_path_buf()).ok())
                .unwrap_or(real)
        } else {
            real
        };
        if let Some(s) = real.to_str() {
            // strip_prefix of the CWD itself yields "" — Go's EvalSymlinks(".")
            // says ".".
            return if s.is_empty() {
                ".".to_string()
            } else {
                s.to_string()
            };
        }
    }
    lexical_clean(p)
}

/// Go's `filepath.Clean` (lexical only).
fn lexical_clean(p: &str) -> String {
    let abs = p.starts_with('/');
    let mut stack: Vec<&str> = Vec::new();
    for part in p.split('/') {
        match part {
            "" | "." => {}
            ".." => {
                if let Some(last) = stack.last() {
                    if *last != ".." {
                        stack.pop();
                        continue;
                    }
                }
                if !abs {
                    stack.push("..");
                }
            }
            seg => stack.push(seg),
        }
    }
    let joined = stack.join("/");
    match (abs, joined.is_empty()) {
        (true, true) => "/".to_string(),
        (true, false) => format!("/{joined}"),
        (false, true) => ".".to_string(),
        (false, false) => joined,
    }
}

/// Whether an event's directory matches the (already-canonical) workdir
/// (`dirMatchCanon`): equal, OR the workdir is under the event directory
/// (opencode may report the project root).
pub(crate) fn dir_match_canon(event_dir: &str, canon_workdir: &str) -> bool {
    if event_dir.is_empty() || canon_workdir.is_empty() {
        return false;
    }
    let ed = canonical_dir(event_dir);
    if ed == canon_workdir {
        return true;
    }
    let prefix = if ed.ends_with('/') {
        ed.clone()
    } else {
        format!("{ed}/")
    };
    canon_workdir.starts_with(&prefix)
}

// ---------------------------------------------------------------------------
// sessionWatcher surface (watch_opencode_transport.go:269-412)
// ---------------------------------------------------------------------------

impl SessionWatcher for OpencodeWatcher {
    /// Drains the inbox under the mutex and folds each payload into the
    /// (single-writer) fold (`refresh`), mirroring the file watcher's refresh
    /// but sourced from the inbox rather than a tailer. A seed-complete
    /// marker flips the transport authoritative; an overflow-gap marker drops
    /// record-exact state. A CLOSED watcher no-ops.
    fn refresh(&self, now: DateTime<Utc>) {
        let mut guard = self.lock();
        let w = &mut *guard;
        if w.closed {
            return;
        }
        let items = std::mem::take(&mut w.inbox);
        w.inbox_bytes = 0;
        // The current-generation seed's status fallback, applied AFTER the batch.
        let mut pending_fb = SeedFallback::default();
        for it in items {
            match it {
                InboxItem::Payload(payload) => {
                    if w.fold.apply_line(&payload) {
                        w.last_event_at = Some(now);
                    }
                }
                InboxItem::SeedComplete { gen, fallback } => {
                    // The seed + buffered replay is now fully folded: the
                    // watcher becomes authoritative. A marker from a
                    // SUPERSEDED generation is ignored (fix #2): connection
                    // A's queued seedComplete must never make connection B
                    // authoritative before B's own seed completes.
                    if gen != w.gen {
                        continue;
                    }
                    w.seed_applied = true;
                    pending_fb = fallback; // applied LAST, below (barrier order)
                }
                InboxItem::OverflowGap { gen } => {
                    // A record was LOST (inbox overflow): drop pending
                    // tool-call state so a swallowed completion can't pin the
                    // verdict at working forever. The dedup set survives
                    // (note_gap keeps it) so the forced reseed emits no
                    // duplicate rows. A gap from a superseded generation is
                    // dropped (fix #2 + #7).
                    if gen != w.gen {
                        continue;
                    }
                    w.fold.note_gap();
                    w.seed_applied = false;
                }
            }
        }
        // The REST /session/status fallback is applied AFTER every payload in
        // this batch (message history AND any buffered live events), so a
        // live boundary in the same batch suppresses it — the barrier is
        // honored LAST and the live stream wins (§3.4, fix #3).
        if pending_fb.set && w.fold.apply_status_fallback(pending_fb.idle) {
            w.last_event_at = Some(now); // the seed established the boundary: count it for freshness
        }
        w.cur_activity = w.fold.activity();
        w.cur_message = w.fold.last_message();
        w.cur_settled = w.fold.settled();
        let msgs = MessageProducer::drain_messages(&mut w.fold);
        w.pending.extend(msgs);
    }

    /// The watcher's verdict + its authority at `now` (`snapshot`). Unlike a
    /// durable file tail, a network watcher's settled verdict is
    /// authoritative ONLY while the transport is healthy: the seed is
    /// applied, the stream is connected, and a frame (or heartbeat) landed
    /// within [`OC_FRAME_STALE_WINDOW`]. When UNHEALTHY it returns BOTH
    /// fresh=false AND expired_working=false — returning only fresh=false
    /// would let mergedActivity keep a stale working verdict against a
    /// churning pane; forcing expired_working=false routes to the
    /// stability-drives branch (§3.6).
    fn snapshot(&self, now: DateTime<Utc>) -> (RcActivity, String, bool, bool) {
        let w = self.lock();
        if w.closed {
            // A closed watcher has revoked its authority (close() cleared
            // connected/seed_applied): never report fresh, and force
            // expired_working=false so pane-stability drives (fix #6).
            return (w.cur_activity, w.cur_message.clone(), false, false);
        }
        let mut healthy = w.seed_applied && w.connected;
        if healthy {
            if let Some(last) = w.last_frame_at {
                let since = now.signed_duration_since(last);
                if since.to_std().is_ok_and(|d| d >= OC_FRAME_STALE_WINDOW) {
                    healthy = false; // heartbeat-stale: the stream is wedged
                }
            }
        }
        if !healthy {
            return (w.cur_activity, w.cur_message.clone(), false, false);
        }
        // Transport healthy: from here the ordinary quiet-source rule applies.
        let (fresh, expired_working) =
            watcher_freshness(w.cur_activity, w.cur_settled, w.last_event_at, now);
        (
            w.cur_activity,
            w.cur_message.clone(),
            fresh,
            expired_working,
        )
    }

    /// Returns and clears the feed messages folded since the last drain
    /// (`drainPending`; stream order).
    fn drain_pending(&self) -> Vec<FeedMessage> {
        std::mem::take(&mut self.lock().pending)
    }

    /// Whether the fold has consumed at least one activity-relevant event
    /// since attach (`hadEvent`).
    fn had_event(&self) -> bool {
        self.lock().last_event_at.is_some()
    }

    /// Marks the watcher terminally closed, revokes authority atomically, and
    /// shuts down the in-flight I/O socket (`close`) — a cancellation flag
    /// alone does NOT unblock a blocked read, so the socket must be shut down
    /// too. NON-BLOCKING (reconcile calls it under the track lock): it NEVER
    /// joins the thread. Idempotent.
    fn close(&self) {
        let (sse_sock, rest_sock) = {
            let mut w = self.lock();
            if w.closed {
                return;
            }
            w.closed = true;
            // Revoke authority atomically with the close (fix #6).
            w.connected = false;
            w.seed_applied = false;
            (w.body.take(), w.rest_body.take())
        };
        self.closed_flag.store(true, Ordering::Relaxed);
        self.wake.notify_all();
        for sock in [sse_sock, rest_sock].into_iter().flatten() {
            let _ = sock.shutdown(std::net::Shutdown::Both);
        }
    }

    fn as_confirmed_agent_id_drainer(&self) -> Option<&dyn ConfirmedAgentIdDrainer> {
        Some(self)
    }

    fn as_approval_publisher(&self) -> Option<&dyn super::watch::ApprovalPublisher> {
        Some(self)
    }

    fn as_approval_blocker(&self) -> Option<&dyn super::watch::ApprovalBlocker> {
        Some(self)
    }

    fn as_turn_starter(&self) -> Option<&dyn super::verbs::TurnStarter> {
        Some(self)
    }

    fn as_turn_interrupter(&self) -> Option<&dyn super::verbs::TurnInterrupter> {
        Some(self)
    }

    fn as_approval_resolver(&self) -> Option<&dyn super::verbs::ApprovalResolver> {
        Some(self)
    }
}

// The verb-lane trait impls (`hub_verbs.go:95-120` — the narrow interfaces
// the handlers type-assert; only this watcher implements them today). Each
// boxes the inherent async method; the handler's own cancellation stands in
// for Go's request context.
impl super::verbs::TurnStarter for OpencodeWatcher {
    fn start_turn<'a>(&'a self, text: &'a str) -> super::verbs::LaneFuture<'a, String> {
        Box::pin(OpencodeWatcher::start_turn(self, text))
    }
}

impl super::verbs::TurnInterrupter for OpencodeWatcher {
    fn interrupt_turn(&self) -> super::verbs::LaneFuture<'_, ()> {
        Box::pin(OpencodeWatcher::interrupt_turn(self))
    }
}

impl super::verbs::ApprovalResolver for OpencodeWatcher {
    fn approval_state(&self, id: &str) -> Option<(String, String)> {
        OpencodeWatcher::approval_state(self, id)
    }
    fn claim_approval(&self, id: &str, decision: &str) -> ApprovalClaim {
        OpencodeWatcher::claim_approval(self, id, decision)
    }
    fn release_approval(&self, id: &str) {
        OpencodeWatcher::release_approval(self, id);
    }
    fn commit_approval(&self, id: &str, decision: &str) -> String {
        OpencodeWatcher::commit_approval(self, id, decision)
    }
    fn resolve_approval<'a>(
        &'a self,
        id: &'a str,
        decision: &'a str,
    ) -> super::verbs::LaneFuture<'a, ()> {
        Box::pin(OpencodeWatcher::resolve_approval(self, id, decision))
    }
}

impl super::watch::ApprovalPublisher for OpencodeWatcher {
    fn pending_approvals(&self) -> Vec<FeedApproval> {
        OpencodeWatcher::pending_approvals(self)
    }
}

impl super::watch::ApprovalBlocker for OpencodeWatcher {
    fn has_open_approvals(&self) -> bool {
        OpencodeWatcher::has_open_approvals(self)
    }
}

impl ConfirmedAgentIdDrainer for OpencodeWatcher {
    /// Returns and clears the SSE-discovered session id the transport pinned
    /// (`drainConfirmedAgentID`; "" when none, already drained, or the pin
    /// came from a prior back-write). The id is enqueued exactly once, so
    /// reconnect+reseed does not re-back-write.
    fn drain_confirmed_agent_id(&self) -> String {
        std::mem::take(&mut self.lock().confirmed_id)
    }
}

// ---------------------------------------------------------------------------
// approvals: the fold state handler threads may touch (transport.go:553-703)
// ---------------------------------------------------------------------------

/// `claimApproval`'s verdict (`approvalClaim`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ApprovalClaim {
    /// This caller owns the resolution and must release or commit it.
    Claimed,
    /// Another request holds the claim right now. Retryable: the caller
    /// answers 409 not_accepting rather than POSTing a second time —
    /// deliberately for a SAME-decision concurrent request too.
    Busy,
    /// The entry is no longer pending (resolved between the handler's state
    /// read and the claim) or unknown. The caller re-reads the state and
    /// answers from it.
    Settled,
}

impl OpencodeWatcher {
    /// A tracked approval's status/decision (`approvalState`), or `None` when
    /// this session never saw the id — the approvals verb's oracle for the
    /// 404-vs-replay-vs-conflict decision. A CLOSED watcher answers
    /// not-found: its session is gone.
    pub fn approval_state(&self, id: &str) -> Option<(String, String)> {
        let w = self.lock();
        if w.closed {
            return None;
        }
        w.fold.approval_state(id)
    }

    /// Records a resolution the HUB performed (`markApprovalResolved` — the
    /// approvals verb's upstream POST returned 2xx) without waiting for
    /// opencode's permission.replied to come back around the stream.
    /// Synchronous on purpose: a same-decision replay arriving before the
    /// next reconcile tick must see the entry already resolved and answer
    /// idempotently instead of re-POSTing. Returns true when THIS call
    /// resolved the entry.
    pub fn mark_approval_resolved(&self, id: &str, decision: &str) -> bool {
        let mut w = self.lock();
        if w.closed {
            return false;
        }
        Self::mark_approval_resolved_locked(&mut w, id, decision)
    }

    fn mark_approval_resolved_locked(w: &mut WatcherState, id: &str, decision: &str) -> bool {
        if !w.fold.resolve_permission(id, decision) {
            return false;
        }
        // Re-derive the cached verdict and collect the resolved row under the
        // SAME lock, so a session whose only blocker was this approval stops
        // reporting needs_approval immediately rather than at the next tick.
        // last_event_at is deliberately NOT stamped: it tracks STREAM
        // evidence, and a local resolve is not a frame.
        w.cur_activity = w.fold.activity();
        w.cur_settled = w.fold.settled();
        let msgs = MessageProducer::drain_messages(&mut w.fold);
        w.pending.extend(msgs);
        true
    }

    /// Atomically transitions a PENDING ask to resolution-in-flight
    /// (`claimApproval`). A closed watcher answers settled.
    pub fn claim_approval(&self, id: &str, decision: &str) -> ApprovalClaim {
        let mut w = self.lock();
        if w.closed {
            return ApprovalClaim::Settled;
        }
        if w.resolving.contains_key(id) {
            return ApprovalClaim::Busy;
        }
        match w.fold.approval_state(id) {
            Some((status, _)) if status == super::messages::APPROVAL_STATUS_PENDING => {}
            _ => return ApprovalClaim::Settled,
        }
        w.resolving.insert(id.to_string(), decision.to_string());
        ApprovalClaim::Claimed
    }

    /// Drops a claim whose upstream POST failed (`releaseApproval`), so the
    /// operator (or a retry) can try again. Does NOT touch the fold: a failed
    /// POST resolved nothing.
    pub fn release_approval(&self, id: &str) {
        self.lock().resolving.remove(id);
    }

    /// Consumes the claim and records the resolution (`commitApproval`),
    /// returning the decision the fold ACTUALLY holds for the id — which the
    /// handler echoes instead of blindly repeating the request's. They differ
    /// in one race: opencode's own permission.replied can land between the
    /// POST and the commit, in which case the STREAM's record wins. A
    /// recorded-but-empty decision falls back to the caller's (the wire shape
    /// requires a non-empty one).
    pub fn commit_approval(&self, id: &str, decision: &str) -> String {
        let mut w = self.lock();
        w.resolving.remove(id);
        if w.closed {
            return decision.to_string();
        }
        Self::mark_approval_resolved_locked(&mut w, id, decision);
        match w.fold.approval_state(id) {
            Some((_, recorded)) if !recorded.is_empty() => recorded,
            _ => decision.to_string(),
        }
    }

    /// The still-open approvals for reconcile to publish
    /// (`pendingApprovals` — the approvalPublisher surface). Freshly
    /// allocated per call.
    pub fn pending_approvals(&self) -> Vec<FeedApproval> {
        let w = self.lock();
        if w.closed {
            return Vec::new();
        }
        w.fold.pending_approvals()
    }

    /// Whether ANY ask (permission or question) is still open, for the input
    /// gate (`hasOpenApprovals` — the approvalBlocker surface). Deliberately
    /// independent of transport health and freshness: when the stream wedges
    /// the activity verdict is demoted to pane stability — but a dialog the
    /// operator has not answered is still on the pane, and a posted line
    /// would answer it by accident. The asymmetry is intentional: a stale
    /// reject costs a retry, a stale accept costs an unintended approval. A
    /// CLOSED watcher blocks nothing.
    pub fn has_open_approvals(&self) -> bool {
        let w = self.lock();
        if w.closed {
            return false;
        }
        w.fold.open_approvals() > 0
    }
}

// ---------------------------------------------------------------------------
// the verb lane: hub-initiated MUTATIONS (transport.go:705-895)
// ---------------------------------------------------------------------------
//
// SESSION-SCOPING INVARIANT (normative — docs/extensions/rc-helper.md): every
// hub-initiated mutation addresses the rc session's PINNED opencode sessionID
// through a session-scoped v1 route. Never a global write route — one TUI's
// embedded server lists sessions from every directory on the machine, so a
// global write can answer a permission belonging to an unrelated project.
// Enforced STRUCTURALLY: these methods take no session parameter, they read
// the pin themselves, and an unpinned session is a 409 rather than a guess.
//
// All three run on the CALLER's task (bounded by OC_VERB_TIMEOUT) and share
// the watcher's async loopback client. They never hold the mutex across the
// POST (the pin is copied out first).
//
// RECREATE WINDOW (accepted): reconcile can replace the tracked session and
// close this watcher while a POST is in flight; the request then targets the
// dead per-create port, fails, and surfaces as a retryable 409.

/// `prompt_async`'s body: the v1 parts array (the v2 route admits a turn but
/// never promotes it on an idle session, so v1 is the control surface).
#[derive(serde::Serialize)]
struct OcPromptRequest {
    parts: Vec<OcPromptPart>,
}

#[derive(serde::Serialize)]
struct OcPromptPart {
    #[serde(rename = "type")]
    typ: &'static str,
    text: String,
}

/// The session-scoped permission reply body (`ocPermissionReply`).
#[derive(serde::Serialize)]
struct OcPermissionReply {
    response: &'static str,
}

/// Maps the contract's decision enum onto opencode's native permission reply
/// vocabulary (`opencodeReplyFromDecision`) — the exact inverse of the fold's
/// `opencode_decision_from_reply`, so a decision we send comes back off the
/// stream as the same decision. `always` is session-scoped in opencode
/// (verified live), which is what makes allow_always safe to forward.
pub(crate) fn opencode_reply_from_decision(decision: &str) -> Option<&'static str> {
    match decision {
        super::messages::APPROVAL_DECISION_ALLOW => Some("once"),
        super::messages::APPROVAL_DECISION_ALLOW_ALWAYS => Some("always"),
        super::messages::APPROVAL_DECISION_DENY => Some("reject"),
        _ => None,
    }
}

impl OpencodeWatcher {
    /// Delivers `text` as one whole turn (`startTurn`): POST
    /// `/session/{pinned}/prompt_async` with a single text part (204). NO
    /// busy check — opencode natively queues/steers typed input while a turn
    /// runs. The returned turn id is HUB-generated and opaque: prompt_async
    /// answers with no body, and clients must not parse the handle.
    pub async fn start_turn(&self, text: &str) -> Result<String, OcWatcherError> {
        let id = self.mut_target()?;
        let body = OcPromptRequest {
            parts: vec![OcPromptPart {
                typ: "text",
                text: text.to_string(),
            }],
        };
        self.post_json(&oc_session_path(&id, &["prompt_async"]), &body)
            .await?;
        Ok(format!("oc-{}", uuid::Uuid::new_v4()))
    }

    /// Aborts the pinned session's running turn (`interruptTurn`): POST
    /// `/session/{pinned}/abort`. The upstream answer is PASSED THROUGH —
    /// opencode answers an abort on an IDLE session successfully too, and the
    /// hub does not second-guess the lane about what is running.
    pub async fn interrupt_turn(&self) -> Result<(), OcWatcherError> {
        let id = self.mut_target()?;
        self.post_json(&oc_session_path(&id, &["abort"]), &serde_json::json!({}))
            .await
    }

    /// Answers one permission ask (`resolveApproval`): POST
    /// `/session/{pinned}/permissions/{id}`. The SESSION-SCOPED route is the
    /// whole point (the global reply route would answer other projects'
    /// asks). The caller has already validated the decision and established
    /// the ask is pending; the local bookkeeping is the caller's too — this
    /// method is purely the upstream write.
    pub async fn resolve_approval(&self, id: &str, decision: &str) -> Result<(), OcWatcherError> {
        let Some(reply) = opencode_reply_from_decision(decision) else {
            return Err(OcWatcherError::Other(format!(
                "unsupported decision {decision:?}"
            )));
        };
        let sid = self.mut_target()?;
        self.post_json(
            &oc_session_path(&sid, &["permissions", id]),
            &OcPermissionReply { response: reply },
        )
        .await
    }

    /// Copies the pinned session id out under the mutex (`mutTarget`) — the
    /// one place a mutation learns its address. A closed watcher is gone; an
    /// uncorrelated one has nothing to address, and guessing is exactly what
    /// the scoping invariant forbids.
    fn mut_target(&self) -> Result<String, OcWatcherError> {
        let w = self.lock();
        if w.closed {
            return Err(OcWatcherError::Closed);
        }
        if w.pinned_id.is_empty() {
            return Err(OcWatcherError::NoAgentSession);
        }
        Ok(w.pinned_id.clone())
    }

    /// The single mutation primitive (`postJSON`): a bounded POST, its body
    /// drained and its status checked. ANY 2xx is success — the three routes
    /// answer 204/200 today, and a version that swaps one for the other has
    /// not changed the outcome. A non-2xx, a timeout, or a closed watcher all
    /// return an error the handler maps to a single retryable 409.
    async fn post_json<T: serde::Serialize>(
        &self,
        path: &str,
        body: &T,
    ) -> Result<(), OcWatcherError> {
        if self.is_closed() {
            return Err(OcWatcherError::Closed);
        }
        let url = format!("http://127.0.0.1:{}{}", self.port, path);
        let resp = self
            .verb_client
            .post(&url)
            .timeout(OC_VERB_TIMEOUT)
            .json(body)
            .send()
            .await
            .map_err(OcWatcherError::other)?;
        let status = resp.status().as_u16();
        // Drain (bounded) so the connection is reusable.
        let _ = resp.bytes().await;
        if !(200..300).contains(&status) {
            return Err(OcWatcherError::Status {
                status,
                path: path.to_string(),
            });
        }
        Ok(())
    }
}

// ---------------------------------------------------------------------------
// the loopback HTTP client (the sanctioned TcpStream fallback — see the
// module doc's callout). Read-side only: the run thread's SSE GET + REST
// seeds. close() unblocks any in-flight read via socket shutdown.
// ---------------------------------------------------------------------------

/// One parsed response head + a framed body reader over the socket.
struct RawResponse {
    status: u16,
    body: BodyReader,
}

/// The response body framing: content-length, chunked, or EOF-delimited
/// (`Connection: close` is always sent, so EOF is a legal terminator).
struct BodyReader {
    stream: std::io::BufReader<TcpStream>,
    framing: Framing,
}

enum Framing {
    /// Remaining bytes.
    Length(u64),
    /// Chunked: `remaining` is what is left of the CURRENT chunk, so
    /// `remaining == 0` means we sit between chunks and must read the next
    /// chunk-size line; `done` latches the terminal 0-sized chunk.
    Chunked {
        remaining: u64,
        done: bool,
    },
    Eof,
}

impl Read for BodyReader {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        match &mut self.framing {
            Framing::Length(remaining) => {
                if *remaining == 0 {
                    return Ok(0);
                }
                let cap = (*remaining).min(buf.len() as u64) as usize;
                let n = self.stream.read(&mut buf[..cap])?;
                *remaining -= n as u64;
                Ok(n)
            }
            Framing::Eof => self.stream.read(buf),
            Framing::Chunked { remaining, done } => {
                if *done {
                    return Ok(0);
                }
                if *remaining == 0 {
                    // Read the next chunk-size line (skipping the previous
                    // chunk's trailing CRLF when present).
                    let mut line = read_crlf_line(&mut self.stream, 1024)?;
                    if line.is_empty() {
                        line = read_crlf_line(&mut self.stream, 1024)?;
                    }
                    let size_str = line.split(';').next().unwrap_or("").trim();
                    let size = u64::from_str_radix(size_str, 16).map_err(|_| {
                        std::io::Error::new(
                            std::io::ErrorKind::InvalidData,
                            format!("bad chunk size {size_str:?}"),
                        )
                    })?;
                    if size == 0 {
                        *done = true;
                        return Ok(0);
                    }
                    *remaining = size;
                }
                let cap = (*remaining).min(buf.len() as u64) as usize;
                let n = self.stream.read(&mut buf[..cap])?;
                *remaining -= n as u64;
                Ok(n)
            }
        }
    }
}

/// Reads one CRLF-terminated line (LF-tolerant), capped at `cap` bytes. EOF
/// before the terminating `\n` is an ERROR, not a short line: a truncated head
/// (or chunk-size line) must surface as a failed read → reconnect, never as a
/// silently-complete one.
fn read_crlf_line(r: &mut impl Read, cap: usize) -> std::io::Result<String> {
    let mut out = Vec::new();
    let mut b = [0u8; 1];
    loop {
        let n = r.read(&mut b)?;
        if n == 0 {
            return Err(std::io::Error::new(
                std::io::ErrorKind::UnexpectedEof,
                "unterminated line",
            ));
        }
        if b[0] == b'\n' {
            break;
        }
        if out.len() >= cap {
            return Err(std::io::Error::new(
                std::io::ErrorKind::InvalidData,
                "header line too long",
            ));
        }
        out.push(b[0]);
    }
    if out.last() == Some(&b'\r') {
        out.pop();
    }
    Ok(String::from_utf8_lossy(&out).into_owned())
}

impl OpencodeWatcher {
    /// Opens a loopback request. `streaming=false` keeps the 5s read deadline
    /// for the whole call (the REST posture); `streaming=true` clears it once
    /// the head is parsed (the SSE GET has NO overall timeout — close()'s
    /// socket shutdown is what unblocks it). The socket is REGISTERED so
    /// close() can shut it down; on a raced close the socket is shut down
    /// here and `Closed` returned (the register/Do race guard, fixes #6/#10).
    fn open_request(&self, path: &str, streaming: bool) -> Result<RawResponse, OcWatcherError> {
        if self.is_closed() {
            return Err(OcWatcherError::Closed);
        }
        let addr = std::net::SocketAddr::from(([127, 0, 0, 1], self.port));
        let stream =
            TcpStream::connect_timeout(&addr, DIAL_TIMEOUT).map_err(OcWatcherError::other)?;
        // The per-read deadline: header-bounded for the stream (cleared once
        // the head is parsed), REST-bounded otherwise. (Go bounds each REST
        // call with an OVERALL 5s context; a per-read deadline on a loopback
        // peer is the same bound in practice — noted as a shape delta.)
        let read_deadline = if streaming {
            HEADER_TIMEOUT
        } else {
            REST_TIMEOUT
        };
        stream
            .set_read_timeout(Some(read_deadline))
            .map_err(OcWatcherError::other)?;
        stream
            .set_write_timeout(Some(HEADER_TIMEOUT))
            .map_err(OcWatcherError::other)?;
        if !self.register_body(&stream, streaming) {
            let _ = stream.shutdown(std::net::Shutdown::Both);
            return Err(OcWatcherError::Closed);
        }
        // Everything past the register is fallible with a REGISTERED socket:
        // any error must deregister it (and shut it down) rather than leave a
        // dead socket in the slot until the next request overwrites it. Go
        // never reaches this state — a failed client.Do registers no body.
        let head = (|| {
            let mut stream = stream;
            // Accept mirrors Go exactly: the SSE GET asks for the event stream
            // (transport.go:973); getJSON sends NO Accept header at all
            // (transport.go:1329-1357), so neither does a non-streaming call.
            let accept = if streaming {
                "Accept: text/event-stream\r\n"
            } else {
                ""
            };
            let req = format!(
                "GET {path} HTTP/1.1\r\nHost: 127.0.0.1:{}\r\n{accept}Connection: close\r\n\r\n",
                self.port
            );
            stream
                .write_all(req.as_bytes())
                .map_err(OcWatcherError::other)?;
            let mut reader = std::io::BufReader::new(stream);
            // Status line + headers. Each line is capped AND the whole head is
            // capped: a peer that streams headers forever would otherwise keep
            // the loop alive indefinitely, since the per-read deadline resets
            // on every line (Go's http.ReadResponse bounds the whole head —
            // DefaultMaxHeaderBytes).
            let mut head_bytes = 0usize;
            let status_line =
                read_crlf_line(&mut reader, 8 * 1024).map_err(OcWatcherError::other)?;
            head_bytes += status_line.len();
            let status: u16 = status_line
                .split_whitespace()
                .nth(1)
                .and_then(|s| s.parse().ok())
                .ok_or_else(|| OcWatcherError::Other(format!("bad status line {status_line:?}")))?;
            let mut content_length: Option<u64> = None;
            let mut chunked = false;
            loop {
                let line = read_crlf_line(&mut reader, 16 * 1024).map_err(OcWatcherError::other)?;
                if line.is_empty() {
                    break;
                }
                head_bytes += line.len();
                if head_bytes > MAX_HEAD_BYTES {
                    return Err(OcWatcherError::Other("response head too large".into()));
                }
                let Some((name, value)) = line.split_once(':') else {
                    continue;
                };
                let name = name.trim().to_ascii_lowercase();
                let value = value.trim();
                if name == "content-length" {
                    content_length = value.parse().ok();
                } else if name == "transfer-encoding"
                    && value.to_ascii_lowercase().contains("chunked")
                {
                    chunked = true;
                }
            }
            if streaming {
                // The SSE stream idles between heartbeats: no read deadline (Go
                // has none either — the stale window at snapshot level is the
                // health bound, and close() shuts the socket down).
                let _ = reader.get_ref().set_read_timeout(None);
            }
            let framing = if chunked {
                Framing::Chunked {
                    remaining: 0,
                    done: false,
                }
            } else if let Some(len) = content_length {
                Framing::Length(len)
            } else {
                Framing::Eof
            };
            Ok(RawResponse {
                status,
                body: BodyReader {
                    stream: reader,
                    framing,
                },
            })
        })();
        if head.is_err() {
            self.clear_body(streaming);
        }
        head
    }

    /// A bounded, deadline-read GET decoded into `out` (`getJSON`). Non-2xx →
    /// error. The close-recheck-after-connect mirrors Go's fix #10: a
    /// raced-but-succeeded request abandons its body immediately rather than
    /// folding post-close work.
    fn get_json<T: serde::de::DeserializeOwned>(&self, path: &str) -> Result<T, OcWatcherError> {
        let mut resp = self.open_request(path, false)?;
        if self.is_closed() {
            self.clear_body(false);
            return Err(OcWatcherError::Closed);
        }
        let result = (|| {
            if resp.status != 200 {
                return Err(OcWatcherError::Other(format!(
                    "GET {path}: status {}",
                    resp.status
                )));
            }
            let mut body = Vec::new();
            (&mut resp.body)
                .take(MAX_REST_BYTES as u64)
                .read_to_end(&mut body)
                .map_err(OcWatcherError::other)?;
            serde_json::from_slice::<T>(&body).map_err(OcWatcherError::other)
        })();
        self.clear_body(false);
        result
    }
}

// ---------------------------------------------------------------------------
// SSE parsing (watch_opencode_transport.go:1400-1478)
// ---------------------------------------------------------------------------

/// Parses an SSE byte stream per the spec (`sseScanner`): accumulates `data:`
/// lines (joined by `\n`), ignores comment lines (leading ':'), ignores the
/// event:/id:/retry: field VALUES, and dispatches one frame on a blank line.
/// Lines are split on `\n` with a trailing `\r` trimmed (CRLF-tolerant). One
/// line and one accumulated frame are both size-capped. `on_line` fires for
/// EVERY scanned line — comment heartbeats and empty-data frames included —
/// so transport freshness tracks any received traffic, not only frames that
/// yield a payload (fix #9).
///
/// Deliberately PRIVATE and separate from `shed_core::sse::SseParser` (plan
/// §2.3, decided): the shared parser drops comment lines and has no per-line
/// callback, so it cannot express the heartbeat-staleness contract.
struct SseScanner<'a, R: Read> {
    reader: R,
    on_line: Box<dyn FnMut() + 'a>,
    buf: Vec<u8>,
    eof: bool,
}

impl<'a, R: Read> SseScanner<'a, R> {
    fn new(reader: R, on_line: Box<dyn FnMut() + 'a>) -> SseScanner<'a, R> {
        SseScanner {
            reader,
            on_line,
            buf: Vec::new(),
            eof: false,
        }
    }

    /// The next line, or None at EOF (`bufio.Scanner` semantics: an oversized
    /// line errors — Go's ErrTooLong).
    fn next_line(&mut self) -> Result<Option<Vec<u8>>, OcWatcherError> {
        loop {
            // The cap is exact, matching bufio.Scanner's ErrTooLong boundary:
            // Go errors the moment `maxTokenSize` bytes are buffered with no
            // newline, so a line of exactly the cap (pre-`\n` bytes, `\r`
            // included) fails and cap-1 passes. Checking only between reads
            // rounded the cap up to the next 4 KiB chunk, and a complete
            // over-cap line already in the buffer slipped through entirely
            // (H8 review MEDIUM).
            if let Some(i) = self.buf.iter().position(|&b| b == b'\n') {
                if i >= MAX_SSE_LINE_BYTES {
                    return Err(OcWatcherError::Other("sse line too long".into()));
                }
                let mut line: Vec<u8> = self.buf.drain(..=i).collect();
                line.pop(); // the \n
                if line.last() == Some(&b'\r') {
                    line.pop(); // ScanLines strips a trailing \r
                }
                return Ok(Some(line));
            }
            if self.buf.len() >= MAX_SSE_LINE_BYTES {
                return Err(OcWatcherError::Other("sse line too long".into()));
            }
            if self.eof {
                // A trailing unterminated line is delivered once, then EOF
                // (bufio.Scanner does the same).
                if self.buf.is_empty() {
                    return Ok(None);
                }
                let mut line = std::mem::take(&mut self.buf);
                if line.last() == Some(&b'\r') {
                    line.pop();
                }
                return Ok(Some(line));
            }
            let mut chunk = [0u8; 4096];
            match self.reader.read(&mut chunk) {
                Ok(0) => self.eof = true,
                Ok(n) => self.buf.extend_from_slice(&chunk[..n]),
                Err(err) => return Err(OcWatcherError::other(err)),
            }
        }
    }

    /// The JSON payload of the next SSE frame (the accumulated `data:`
    /// fields), or an error (EOF / oversized / the body's read error — e.g.
    /// after close()).
    fn next(&mut self) -> Result<Vec<u8>, OcWatcherError> {
        let mut data: Vec<u8> = Vec::new();
        loop {
            let Some(line) = self.next_line()? else {
                return Err(OcWatcherError::Other("eof".into()));
            };
            (self.on_line)(); // any received line is transport activity (fix #9)
            if line.is_empty() {
                // Blank line → dispatch if we accumulated any data; otherwise
                // keep reading (leading blank lines / keep-alive newlines).
                if !data.is_empty() {
                    return Ok(data);
                }
                continue;
            }
            if line[0] == b':' {
                continue; // comment line
            }
            let (field, value) = split_sse_field(&line);
            if field != b"data" {
                continue; // event:/id:/retry:/unknown — value ignored
            }
            if !data.is_empty() {
                data.push(b'\n'); // multiple data: lines join with \n (SSE spec)
            }
            data.extend_from_slice(value);
            if data.len() > MAX_SSE_FRAME_BYTES {
                return Err(OcWatcherError::SseOversize);
            }
        }
    }
}

/// Splits an SSE line into (field, value): everything before the first ':' is
/// the field; the value is what follows, with a single leading space stripped
/// (`splitSSEField`). A line with no ':' is a field with an empty value.
fn split_sse_field(line: &[u8]) -> (&[u8], &[u8]) {
    match line.iter().position(|&b| b == b':') {
        None => (line, &[]),
        Some(i) => {
            let mut v = &line[i + 1..];
            if v.first() == Some(&b' ') {
                v = &v[1..];
            }
            (&line[..i], v)
        }
    }
}

// ---------------------------------------------------------------------------
// the I/O thread: correlation state machine + SSE read + REST seed
// (watch_opencode_transport.go:920-1398)
// ---------------------------------------------------------------------------

/// Advances the reconnect backoff (`nextReconnectBackoff`). The floor is
/// restored ONLY after a SUCCESSFUL seed — a connection that reached
/// server.connected but then failed its REST seed or immediately EOF'd keeps
/// GROWING the backoff (fix #8). Exponential, capped; jitter is applied
/// separately in the sleep.
pub(crate) fn next_reconnect_backoff(cur: Duration, seeded_ok: bool) -> Duration {
    if seeded_ok {
        return OC_BACKOFF_BASE;
    }
    (cur * 2).min(OC_BACKOFF_MAX)
}

/// The lightweight decode the transport uses for ROUTING only (`ocPeek` —
/// pin/filter; the fold does the real parse).
#[derive(Debug, Default, Deserialize)]
struct OcPeek {
    #[serde(default, rename = "type", deserialize_with = "null_default")]
    typ: String,
    #[serde(default, deserialize_with = "object_opt")]
    properties: Option<OcPeekProps>,
}

#[derive(Debug, Default, Deserialize)]
struct OcPeekProps {
    #[serde(default, rename = "sessionID", deserialize_with = "null_default")]
    session_id: String,
    #[serde(default, deserialize_with = "object_opt")]
    info: Option<OcPeekInfo>,
}

#[derive(Debug, Default, Deserialize)]
struct OcPeekInfo {
    #[serde(default, deserialize_with = "null_default")]
    id: String,
    #[serde(default, rename = "parentID", deserialize_with = "null_default")]
    parent_id: String,
    #[serde(default, deserialize_with = "null_default")]
    directory: String,
}

impl OcPeek {
    fn session_id(&self) -> &str {
        self.properties
            .as_ref()
            .map(|p| p.session_id.as_str())
            .unwrap_or("")
    }
}

/// Decodes a payload for routing (`peekEnvelope`; never fails — an
/// unparseable payload yields an empty-type peek, which routes as "ignore").
fn peek_envelope(payload: &[u8]) -> OcPeek {
    if json_first_byte(payload) != Some(b'{') {
        return OcPeek::default();
    }
    serde_json::from_slice(payload).unwrap_or_default()
}

/// Whether an envelope type is one the fold consumes (`foldRelevantType` — so
/// only those are pushed to the inbox; server.*/session.created/step-* etc.
/// update last_frame_at for the heartbeat/pin logic but never enter the
/// fold). The reply/rejected types are as load-bearing as the asked ones:
/// without them an approval would never be retired from the fold.
pub(crate) fn fold_relevant_type(typ: &str) -> bool {
    matches!(
        typ,
        "session.status"
            | "session.idle"
            | "message.updated"
            | "message.part.updated"
            | "permission.asked"
            | "permission.replied"
            | "question.asked"
            | "question.replied"
            | "question.rejected"
    )
}

// ---- REST seed shapes (transport.go:1231-1326) ----

/// One `{info, parts}` element of GET /session/{id}/message (`restMessage` —
/// raw pass-through so the exact opencode shapes reach the fold unchanged).
#[derive(Debug, Deserialize)]
struct RestMessage {
    #[serde(default, deserialize_with = "raw_opt")]
    info: Option<Box<RawValue>>,
    #[serde(default, deserialize_with = "null_default")]
    parts: Vec<Box<RawValue>>,
}

#[derive(Debug, Default, Deserialize)]
struct RestStatusEntry {
    #[serde(default, rename = "type", deserialize_with = "null_default")]
    typ: String,
}

#[derive(Debug, Deserialize)]
struct RestPermission {
    #[serde(default, deserialize_with = "null_default")]
    id: String,
    #[serde(default, rename = "sessionID", deserialize_with = "null_default")]
    session_id: String,
    #[serde(default, deserialize_with = "null_default")]
    permission: String,
    #[serde(default, deserialize_with = "null_default")]
    patterns: Vec<String>,
    /// Passed through so a seeded row carries the same tool detail as a live one.
    #[serde(default, deserialize_with = "raw_opt")]
    metadata: Option<Box<RawValue>>,
}

#[derive(Debug, Deserialize)]
struct RestQuestion {
    #[serde(default, deserialize_with = "null_default")]
    id: String,
    #[serde(default, rename = "sessionID", deserialize_with = "null_default")]
    session_id: String,
    #[serde(default, deserialize_with = "raw_opt")]
    questions: Option<Box<RawValue>>,
}

#[derive(Debug, Deserialize)]
struct RestSessionEntry {
    #[serde(default, deserialize_with = "null_default")]
    id: String,
    #[serde(default, deserialize_with = "null_default")]
    directory: String,
    #[serde(default, rename = "parentID", deserialize_with = "null_default")]
    parent_id: String,
}

// ---- synthesized-envelope emission (transport.go:1359-1398) ----

/// A `{type,properties}` frame built from REST data that folds through the
/// exact same apply_line path as a live SSE frame (`synthEnvelope`), so seed
/// history is deduped against live events by partID/callID.
#[derive(serde::Serialize)]
struct SynthEnvelope<'a> {
    #[serde(rename = "type")]
    typ: &'a str,
    properties: SynthProps<'a>,
}

/// `synthProps`. Every optional field is skipped when unset (Go's
/// `omitempty`), and the four passthroughs borrow their `RawValue` so the
/// original REST bytes are emitted verbatim — a seeded row carries exactly
/// the shape opencode served.
#[derive(serde::Serialize, Default)]
struct SynthProps<'a> {
    #[serde(rename = "sessionID")]
    session_id: &'a str,
    #[serde(skip_serializing_if = "Option::is_none")]
    info: Option<&'a RawValue>,
    #[serde(skip_serializing_if = "Option::is_none")]
    part: Option<&'a RawValue>,
    #[serde(skip_serializing_if = "Option::is_none")]
    id: Option<&'a str>,
    #[serde(skip_serializing_if = "Option::is_none")]
    permission: Option<&'a str>,
    #[serde(skip_serializing_if = "Option::is_none")]
    patterns: Option<&'a [String]>,
    #[serde(skip_serializing_if = "Option::is_none")]
    metadata: Option<&'a RawValue>,
    #[serde(skip_serializing_if = "Option::is_none")]
    questions: Option<&'a RawValue>,
    /// The approval-seed marker's payload (`OC_APPROVAL_SEED_TYPE` only). The
    /// AUTHORITY is the per-half Known flag, never the presence of a list: an
    /// absent list decodes as the empty set, which is the "nothing is open on
    /// the server" statement that retires a locally-open ask — and that
    /// statement counts only when its half's REST read actually succeeded.
    /// The skips keep every other synthesized envelope free of these fields.
    #[serde(rename = "permissionIDs", skip_serializing_if = "Option::is_none")]
    permission_ids: Option<&'a [String]>,
    #[serde(rename = "questionIDs", skip_serializing_if = "Option::is_none")]
    question_ids: Option<&'a [String]>,
    #[serde(rename = "permissionsKnown", skip_serializing_if = "is_false")]
    permissions_known: bool,
    #[serde(rename = "questionsKnown", skip_serializing_if = "is_false")]
    questions_known: bool,
}

/// Go's `omitempty` for a bool.
fn is_false(b: &bool) -> bool {
    !*b
}

impl OpencodeWatcher {
    /// Serializes a synthesized envelope and pushes it onto the inbox
    /// (`pushSynth`). Returns false on overflow (the caller aborts the seed to
    /// a reconnect). A serialization failure is DROPPED (true) — like Go, an
    /// un-marshalable synth is not an overflow.
    fn push_synth(&self, typ: &str, props: SynthProps<'_>) -> bool {
        let env = SynthEnvelope {
            typ,
            properties: props,
        };
        match serde_json::to_vec(&env) {
            Ok(raw) => self.push_payload(raw),
            Err(_) => true,
        }
    }

    /// The single thread that owns all HTTP I/O (`run`). Loops
    /// connect→(pin)→seed→live and, on any disconnect/error/overflow, marks
    /// disconnected and reconnects with jittered backoff. The fold is NEVER
    /// mutated here — only the inbox + health flags + confirmed-id slot.
    fn run(&self) {
        let mut backoff = OC_BACKOFF_BASE;
        self.set_backoff(backoff);
        loop {
            if self.is_closed() {
                break;
            }
            let (seeded_ok, err) = self.connect_and_stream();
            if self.is_closed() {
                break;
            }
            if let Some(err) = err {
                if err != OcWatcherError::Closed {
                    (self.logf)(&format!(
                        "rc hub: opencode watcher :{} stream ended: {err}",
                        self.port
                    ));
                }
            }
            self.set_connected(false);
            backoff = next_reconnect_backoff(backoff, seeded_ok);
            self.set_backoff(backoff);
            if !self.sleep_backoff(backoff) {
                break; // closed
            }
        }
        // fix #6: on any exit path, revoke authority (also clears seed_applied).
        self.set_connected(false);
    }

    /// Waits a jittered duration in [d/2, d] or returns false on close
    /// (`sleepBackoff`). The only wait in the loop. (Go draws full jitter over
    /// the whole window via math/rand; the nanosecond clock used here carries
    /// under 1s of entropy, so past d = 2s the draw covers only the first ~1s
    /// of the window — still ample spreading, which is all the jitter is for.)
    fn sleep_backoff(&self, d: Duration) -> bool {
        let d = if d.is_zero() { OC_BACKOFF_BASE } else { d };
        let half = d / 2;
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.subsec_nanos() as u64)
            .unwrap_or(0);
        let jitter = Duration::from_nanos(nanos % (half.as_nanos() as u64 + 1));
        let jittered = half + jitter;
        let guard = self
            .wake_mu
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let (_guard, _res) = self
            .wake
            .wait_timeout_while(guard, jittered, |()| {
                !self.closed_flag.load(Ordering::Relaxed)
            })
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        !self.is_closed()
    }

    /// Opens /event, runs the correlation state machine, seeds, and folds the
    /// live stream until it ends/errors (`connectAndStream`). `seeded_ok`
    /// reports whether a seed reached its barrier this connection — the only
    /// thing that resets the reconnect backoff (fix #8).
    fn connect_and_stream(&self) -> (bool, Option<OcWatcherError>) {
        // Advance the connection generation: any stale queued marker is
        // invalidated (fix #2).
        self.begin_generation();
        let resp = match self.open_request("/event", true) {
            Ok(r) => r,
            Err(err) => return (false, Some(err)),
        };
        if resp.status != 200 {
            // Non-2xx (401 when a password somehow reached opencode, 5xx):
            // disconnect + backoff (degrade to stability visibly, never a
            // silent hot-loop).
            self.clear_body(true);
            return (
                false,
                Some(OcWatcherError::Other(format!(
                    "GET /event: status {}",
                    resp.status
                ))),
            );
        }

        let mut seeded_ok = false;
        // mark_frame fires on EVERY scanned line (comment heartbeats + empty
        // frames included, fix #9).
        let mut scanner = SseScanner::new(resp.body, Box::new(|| self.mark_frame()));
        // prior_id or a previously-discovered id survives across reconnects.
        let mut pinned = self.get_pinned();
        let mut candidate = String::new(); // a REST follow-only candidate, unconfirmed
        let mut candidate_tried = false;
        let mut candidate_fallback = SeedFallback::default();
        let mut seeded = false;

        let result = loop {
            let payload = match scanner.next() {
                Ok(p) => p,
                Err(err) => break Some(err),
            };
            let pk = peek_envelope(&payload);

            if pk.typ == "server.connected" {
                self.set_connected(true);
                if !pinned.is_empty() {
                    if let Err(serr) = self.seed_and_barrier(&pinned) {
                        break Some(serr);
                    }
                    seeded = true;
                    seeded_ok = true;
                } else if !candidate_tried {
                    candidate_tried = true;
                    // A follow-only candidate seed failure must NOT be
                    // swallowed: incomplete history could later be blindly
                    // declared authoritative by an SSE confirm (fix #4).
                    match self.establish_candidate() {
                        Ok(Some((cand, fb))) => {
                            candidate = cand;
                            candidate_fallback = fb;
                        }
                        Ok(None) => {}
                        Err(cerr) => break Some(cerr),
                    }
                }
            } else if pinned.is_empty() {
                // Searching: pin only from port-local SSE evidence (§3.3).
                if let Some(id) = self.root_pin_from_created(&pk) {
                    pinned = id.clone();
                    self.set_pinned(&id, true);
                    if let Err(serr) = self.seed_and_barrier(&id) {
                        break Some(serr);
                    }
                    seeded = true;
                    seeded_ok = true;
                } else if !candidate.is_empty() && pk.session_id() == candidate {
                    // The follow-only candidate is now confirmed by a live
                    // event on OUR stream. Fold the confirming event FIRST,
                    // then the barrier LAST (§3.4 order, fix #3).
                    pinned = candidate.clone();
                    self.set_pinned(&candidate, true);
                    if fold_relevant_type(&pk.typ) && !self.push_payload(payload) {
                        break Some(OcWatcherError::InboxOverflow);
                    }
                    // History was already follow-only seeded.
                    self.push_seed_complete(candidate_fallback);
                    seeded = true;
                    seeded_ok = true;
                }
            } else {
                // Live: filter to the pinned session (drop child/sibling ids)
                // and fold.
                if !seeded {
                    // Pinned-but-unseeded can only happen if the seed failed
                    // earlier; re-seed defensively before folding live frames.
                    if let Err(serr) = self.seed_and_barrier(&pinned) {
                        break Some(serr);
                    }
                    seeded = true;
                    seeded_ok = true;
                }
                let sid = pk.session_id();
                if !sid.is_empty() && sid != pinned {
                    // another session's event: drop
                } else if fold_relevant_type(&pk.typ) && !self.push_payload(payload) {
                    break Some(OcWatcherError::InboxOverflow); // overflow → forced reconnect+reseed
                }
            }

            if self.is_closed() {
                break Some(OcWatcherError::Closed);
            }
        };
        self.clear_body(true);
        (seeded_ok, result)
    }

    /// Seeds the pinned session's history + status/permission/question via
    /// REST, then enqueues the seed-complete barrier (`seedAndBarrier`).
    /// Reseed is idempotent (the fold's dedup survives a reconnect). A seed
    /// error (including a failed /session/status, fix #5) propagates so the
    /// caller forces a reconnect+reseed.
    fn seed_and_barrier(&self, id: &str) -> Result<(), OcWatcherError> {
        let fb = self.seed_history(id)?;
        self.push_seed_complete(fb);
        Ok(())
    }

    /// Reconstructs the pinned session's state from REST and pushes it as
    /// synthesized envelopes (`seedHistory`): GET /session/{id}/message →
    /// message.updated + message.part.updated; GET /permission + /question →
    /// filtered asks + the approval-seed marker. GET /session/status
    /// establishes the activity boundary, returned as a FALLBACK applied by
    /// refresh only if no live boundary was folded (fix #3). Both the message
    /// read AND the status read are fatal on error (fix #5). Overflow during
    /// seed aborts to a reconnect.
    fn seed_history(&self, id: &str) -> Result<SeedFallback, OcWatcherError> {
        let msgs: Vec<RestMessage> = self.get_json(&oc_session_path(id, &["message"]))?;
        for m in &msgs {
            if m.info.is_some()
                && !self.push_synth(
                    "message.updated",
                    SynthProps {
                        session_id: id,
                        info: m.info.as_deref(),
                        ..SynthProps::default()
                    },
                )
            {
                return Err(OcWatcherError::InboxOverflow);
            }
            for part in &m.parts {
                if !self.push_synth(
                    "message.part.updated",
                    SynthProps {
                        session_id: id,
                        part: Some(&**part),
                        ..SynthProps::default()
                    },
                ) {
                    return Err(OcWatcherError::InboxOverflow);
                }
            }
        }

        // Status seed: a present-and-complete status map decides idle-vs-busy
        // (absence of the id in a 200 map == idle — opencode omits idle
        // sessions). A FAILED read means the boundary could not be
        // established → the whole seed fails (fix #5).
        let status: Result<HashMap<String, RestStatusEntry>, _> = self.get_json("/session/status");
        let Ok(status) = status else {
            return Err(OcWatcherError::StatusSeedFailed);
        };
        let fb = match status.get(id) {
            None => SeedFallback {
                set: true,
                idle: true,
            },
            Some(st) => SeedFallback {
                set: true,
                idle: st.typ == "idle",
            },
        };

        // Open asks: replay each one (deduped by the fold), collecting its
        // id, then push the approval-seed marker carrying the authoritative
        // open-id sets. The two reads carry INDEPENDENT authority: whichever
        // succeeded marks its half known, so a FAILED read is never folded as
        // "nothing is open" AND never blocks the other half from healing.
        let perms: Result<Vec<RestPermission>, _> = self.get_json("/permission");
        let (perms, perms_ok) = match perms {
            Ok(all) => (
                all.into_iter()
                    .filter(|p| p.session_id == id)
                    .collect::<Vec<_>>(),
                true,
            ),
            Err(_) => (Vec::new(), false),
        };
        let mut perm_ids = Vec::with_capacity(perms.len());
        for p in perms {
            if !self.push_synth(
                "permission.asked",
                SynthProps {
                    session_id: id,
                    id: Some(&p.id),
                    permission: Some(&p.permission),
                    patterns: Some(&p.patterns),
                    metadata: p.metadata.as_deref(),
                    ..SynthProps::default()
                },
            ) {
                return Err(OcWatcherError::InboxOverflow);
            }
            perm_ids.push(p.id);
        }
        let questions: Result<Vec<RestQuestion>, _> = self.get_json("/question");
        let (questions, questions_ok) = match questions {
            Ok(all) => (
                all.into_iter()
                    .filter(|q| q.session_id == id)
                    .collect::<Vec<_>>(),
                true,
            ),
            Err(_) => (Vec::new(), false),
        };
        let mut ques_ids = Vec::with_capacity(questions.len());
        for q in questions {
            if !self.push_synth(
                "question.asked",
                SynthProps {
                    session_id: id,
                    id: Some(&q.id),
                    questions: q.questions.as_deref(),
                    ..SynthProps::default()
                },
            ) {
                return Err(OcWatcherError::InboxOverflow);
            }
            ques_ids.push(q.id);
        }
        if (perms_ok || questions_ok)
            && !self.push_synth(
                OC_APPROVAL_SEED_TYPE,
                SynthProps {
                    session_id: id,
                    permission_ids: Some(&perm_ids),
                    question_ids: Some(&ques_ids),
                    permissions_known: perms_ok,
                    questions_known: questions_ok,
                    ..SynthProps::default()
                },
            )
        {
            return Err(OcWatcherError::InboxOverflow);
        }
        Ok(fb)
    }

    /// Consults GET /session for a follow-only candidate
    /// (`establishCandidate`) — the newest ROOT session whose canonical
    /// directory matches the workdir — and, if found, seeds its history
    /// follow-only (feed populated, but NO confirmed id and NO seed-complete
    /// barrier, so activity stays non-authoritative until a live SSE event on
    /// our own stream confirms it). GET /session is the shared DB and is NOT
    /// a trusted pin on its own. A seed FAILURE is propagated (fix #4).
    /// `Ok(None)` when no candidate matches (an idle TUI with no session yet
    /// stays watchable indefinitely).
    fn establish_candidate(&self) -> Result<Option<(String, SeedFallback)>, OcWatcherError> {
        let Some(id) = self.rest_find_candidate() else {
            return Ok(None);
        };
        // Follow-only: fold the history but do NOT barrier/confirm.
        let fb = self.seed_history(&id)?;
        Ok(Some((id, fb)))
    }

    /// Picks the newest ROOT session (empty parentID) whose canonical
    /// directory matches the workdir from GET /session
    /// (`restFindCandidate`; the list is sorted most-recently-updated first,
    /// so the first match is newest).
    fn rest_find_candidate(&self) -> Option<String> {
        let sessions: Vec<RestSessionEntry> = self.get_json("/session").ok()?;
        for s in sessions {
            if !valid_opencode_session_id(&s.id) || !s.parent_id.is_empty() {
                continue; // same rule as the SSE pin: an unaddressable id is never followed
            }
            if dir_match_canon(&s.directory, &self.workdir) {
                return Some(s.id);
            }
        }
        None
    }

    /// A trusted pin from a session.created/session.updated frame on our own
    /// (directory-scoped) stream (`rootPinFromCreated`): the session must be
    /// a ROOT (empty parentID) whose canonical directory matches (equals, or
    /// is an ancestor of) the workdir.
    fn root_pin_from_created(&self, pk: &OcPeek) -> Option<String> {
        if pk.typ != "session.created" && pk.typ != "session.updated" {
            return None;
        }
        let info = pk.properties.as_ref()?.info.as_ref()?;
        if !valid_opencode_session_id(&info.id) || !info.parent_id.is_empty() {
            return None; // a malformed id is not addressable, so it is not a pin
        }
        if !dir_match_canon(&info.directory, &self.workdir) {
            return None;
        }
        Some(info.id.clone())
    }
}

// ---------------------------------------------------------------------------
// fakeOpencode — the programmable stand-in for opencode's embedded HTTP+SSE
// server (watch_opencode_transport_test.go:46-380), ported as a first-class
// test asset (plan §3): /event SSE + the REST seed endpoints + the three
// mutation routes, with per-route status overrides, request recording, gates,
// and the WS-B pinGuard. Test-only, like Go's _test.go home.
// ---------------------------------------------------------------------------

#[cfg(test)]
pub(crate) mod fake {
    use super::*;
    use std::io::BufRead;
    use std::net::{TcpListener, TcpStream};
    use std::sync::atomic::{AtomicI64, Ordering};

    /// One recorded request against the fake (`ocRequest`).
    #[derive(Debug, Clone)]
    pub(crate) struct OcRequest {
        pub method: String,
        pub path: String,
        pub body: String,
    }

    /// One live /event connection handed to `on_event` (Go passes
    /// `(w, flush, ctx)`; the ctx-done signal here is client disconnect,
    /// observable via [`SseConn::disconnected`]).
    pub(crate) struct SseConn {
        stream: TcpStream,
    }

    impl SseConn {
        /// `writeSSE`: one opencode frame (event name is always "message").
        pub fn write_sse(&mut self, json_payload: &str) {
            let _ = self
                .stream
                .write_all(format!("event: message\ndata: {json_payload}\n\n").as_bytes());
        }

        /// A raw write (oversized-frame / comment-heartbeat shapes).
        pub fn write_raw(&mut self, s: &str) {
            let _ = self.stream.write_all(s.as_bytes());
        }

        /// Whether the client hung up (the Go handlers' ctx.Done()): a
        /// non-blocking read answers Ok(0)/reset on a closed peer and
        /// WouldBlock on a live one (the client sends nothing after the
        /// request head).
        pub fn disconnected(&self) -> bool {
            let _ = self.stream.set_nonblocking(true);
            let mut buf = [0u8; 1];
            let gone = match self.stream.peek(&mut buf) {
                Ok(0) => true,
                Ok(_) => false,
                Err(err) => err.kind() != std::io::ErrorKind::WouldBlock,
            };
            let _ = self.stream.set_nonblocking(false);
            gone
        }

        /// `<-ctx.Done()`: hold the connection open until the client goes
        /// away (or the fake shuts down).
        pub fn hold_until_disconnect(&self) {
            while !self.disconnected() {
                std::thread::sleep(Duration::from_millis(5));
            }
        }
    }

    type OnEvent = Arc<dyn Fn(i64, &mut SseConn) + Send + Sync>;
    type Gate = Arc<dyn Fn(i64, &SseConn) + Send + Sync>;
    type MutGate = Arc<dyn Fn(&str) + Send + Sync>;

    #[derive(Default)]
    struct FakeConfig {
        session_body: String,
        messages_body: String,
        status_body: String,
        permission_body: String,
        question_body: String,
        event_status: u16,
        messages_status: u16,
        status_status: u16,
        question_status: u16,
        prompt_status: u16,
        abort_status: u16,
        permission_status: u16,
        pin_guard: String,
        requests: Vec<OcRequest>,
        /// The WS-B guard's findings (Go fails the test from the handler;
        /// here tests assert this stays empty).
        violations: Vec<String>,
        on_event: Option<OnEvent>,
        before_messages: Option<Gate>,
        before_mutation: Option<MutGate>,
    }

    pub(crate) struct FakeOpencode {
        port: u16,
        cfg: Arc<Mutex<FakeConfig>>,
        pub event_conns: Arc<AtomicI64>,
        pub session_hits: Arc<AtomicI64>,
        pub messages_hits: Arc<AtomicI64>,
        listener: TcpListener,
    }

    /// The ONLY shape a hub-initiated mutation may take
    /// (`ocScopedMutationRe`): POST /session/{id}/(prompt_async|abort|
    /// permissions/{permID}). Anything else a POST could reach is an
    /// invariant violation by construction.
    pub(crate) fn mutation_violation(path: &str, pin: &str) -> Option<String> {
        // A path that is not even /session/<a>/<b> IS a violation — Go's
        // regex fails to match /permission/{id}/reply and friends and reports
        // them; a `?` early-return here would silently pass exactly the
        // global routes the WS-B invariant exists to catch (H8 review HIGH).
        let Some((sid, tail)) = path
            .strip_prefix("/session/")
            .and_then(|rest| rest.split_once('/'))
        else {
            return Some(format!("not a session-scoped mutation route: POST {path}"));
        };
        let scoped = tail == "prompt_async"
            || tail == "abort"
            || (tail.starts_with("permissions/")
                && !tail["permissions/".len()..].is_empty()
                && !tail["permissions/".len()..].contains('/'));
        if !scoped || sid.is_empty() {
            return Some(format!("not a session-scoped mutation route: POST {path}"));
        }
        if !pin.is_empty() && sid != pin {
            return Some(format!("addressed session {sid}, not the pinned {pin}"));
        }
        None
    }

    impl FakeOpencode {
        pub fn new() -> Arc<FakeOpencode> {
            let listener = TcpListener::bind("127.0.0.1:0").expect("bind fake opencode");
            let port = listener.local_addr().expect("addr").port();
            let f = Arc::new(FakeOpencode {
                port,
                cfg: Arc::new(Mutex::new(FakeConfig {
                    session_body: "[]".into(),
                    messages_body: "[]".into(),
                    status_body: "{}".into(),
                    permission_body: "[]".into(),
                    question_body: "[]".into(),
                    ..FakeConfig::default()
                })),
                event_conns: Arc::new(AtomicI64::new(0)),
                session_hits: Arc::new(AtomicI64::new(0)),
                messages_hits: Arc::new(AtomicI64::new(0)),
                listener,
            });
            let srv = Arc::clone(&f);
            let accept = srv.listener.try_clone().expect("clone listener");
            std::thread::spawn(move || {
                for conn in accept.incoming() {
                    let Ok(conn) = conn else { break };
                    let srv = Arc::clone(&srv);
                    std::thread::spawn(move || srv.serve_conn(conn));
                }
            });
            f
        }

        pub fn port(&self) -> u16 {
            self.port
        }

        pub fn set<F: FnOnce(&mut FakeSetter<'_>)>(&self, f: F) {
            let mut cfg = self
                .cfg
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            f(&mut FakeSetter { cfg: &mut cfg });
        }

        pub fn on_event(&self, h: impl Fn(i64, &mut SseConn) + Send + Sync + 'static) {
            self.cfg
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .on_event = Some(Arc::new(h));
        }

        /// `holdOpenSSE`: server.connected then hold — unless an on_event was
        /// already installed.
        pub fn hold_open_sse(&self) {
            let mut cfg = self
                .cfg
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            if cfg.on_event.is_some() {
                return;
            }
            cfg.on_event = Some(Arc::new(|_conn, c: &mut SseConn| {
                c.write_sse(SSE_SERVER_CONNECTED);
                c.hold_until_disconnect();
            }));
        }

        /// `streamAsk`: server.connected + ONE permission.asked, then hold.
        pub fn stream_ask(&self, sid: &str, id: &str) {
            let ask = permission_asked(sid, id, Some(r#"{"command":"ls -la"}"#));
            self.on_event(move |_conn, c| {
                c.write_sse(SSE_SERVER_CONNECTED);
                c.write_sse(&ask);
                c.hold_until_disconnect();
            });
        }

        pub fn before_messages(&self, h: impl Fn(i64, &SseConn) + Send + Sync + 'static) {
            self.cfg
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .before_messages = Some(Arc::new(h));
        }

        pub fn before_mutation(&self, h: impl Fn(&str) + Send + Sync + 'static) {
            self.cfg
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .before_mutation = Some(Arc::new(h));
        }

        pub fn post_paths(&self) -> Vec<String> {
            self.paths("POST")
        }

        pub fn get_paths(&self) -> Vec<String> {
            self.paths("GET")
        }

        fn paths(&self, method: &str) -> Vec<String> {
            self.cfg
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .requests
                .iter()
                .filter(|r| r.method == method)
                .map(|r| r.path.clone())
                .collect()
        }

        pub fn post_body(&self, suffix: &str) -> String {
            self.cfg
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .requests
                .iter()
                .find(|r| r.method == "POST" && r.path.ends_with(suffix))
                .map(|r| r.body.clone())
                .unwrap_or_default()
        }

        /// The WS-B guard's findings — must be empty at every test's end (the
        /// Go fake fails the test from the handler; a Rust server thread
        /// cannot, so tests assert this instead).
        pub fn violations(&self) -> Vec<String> {
            self.cfg
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .violations
                .clone()
        }

        fn serve_conn(&self, stream: TcpStream) {
            let _ = stream.set_read_timeout(Some(Duration::from_secs(10)));
            let mut reader = std::io::BufReader::new(stream.try_clone().expect("clone conn"));
            let mut request_line = String::new();
            if reader.read_line(&mut request_line).is_err() {
                return;
            }
            let mut parts = request_line.split_whitespace();
            let (Some(method), Some(target)) = (parts.next(), parts.next()) else {
                return;
            };
            let (method, target) = (method.to_string(), target.to_string());
            let path = target.split('?').next().unwrap_or("").to_string();
            let mut content_length = 0usize;
            loop {
                let mut line = String::new();
                if reader.read_line(&mut line).is_err() {
                    return;
                }
                let line = line.trim_end();
                if line.is_empty() {
                    break;
                }
                if let Some((name, value)) = line.split_once(':') {
                    if name.trim().eq_ignore_ascii_case("content-length") {
                        content_length = value.trim().parse().unwrap_or(0);
                    }
                }
            }
            let mut body = vec![0u8; content_length.min(1 << 20)];
            if content_length > 0 && reader.read_exact(&mut body).is_err() {
                return;
            }
            let body = String::from_utf8_lossy(&body).into_owned();

            // recordAndGuard: record every request; a violating POST is
            // recorded as a violation and answered 500.
            let violation = {
                let mut cfg = self
                    .cfg
                    .lock()
                    .unwrap_or_else(std::sync::PoisonError::into_inner);
                cfg.requests.push(OcRequest {
                    method: method.clone(),
                    path: path.clone(),
                    body: body.clone(),
                });
                let pin = cfg.pin_guard.clone();
                let violation = if method == "POST" {
                    mutation_violation(&path, &pin)
                } else {
                    None
                };
                if let Some(v) = &violation {
                    cfg.violations.push(v.clone());
                }
                violation
            };
            let mut out = stream;
            if violation.is_some() {
                respond(&mut out, 500, "");
                return;
            }

            match (method.as_str(), path.as_str()) {
                ("GET", "/event") => {
                    let conn = self.event_conns.fetch_add(1, Ordering::SeqCst) + 1;
                    let (status, handler) = {
                        let cfg = self
                            .cfg
                            .lock()
                            .unwrap_or_else(std::sync::PoisonError::into_inner);
                        (cfg.event_status, cfg.on_event.clone())
                    };
                    if status != 0 {
                        respond(&mut out, status, "");
                        return;
                    }
                    let _ = out.write_all(
                        b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n",
                    );
                    if let Some(h) = handler {
                        let mut conn_w = SseConn { stream: out };
                        h(conn, &mut conn_w);
                    }
                }
                ("GET", "/session/status") => {
                    let (body, status) = {
                        let cfg = self
                            .cfg
                            .lock()
                            .unwrap_or_else(std::sync::PoisonError::into_inner);
                        (cfg.status_body.clone(), cfg.status_status)
                    };
                    if status != 0 {
                        respond(&mut out, status, "");
                    } else {
                        respond(&mut out, 200, &body);
                    }
                }
                ("GET", "/session") => {
                    self.session_hits.fetch_add(1, Ordering::SeqCst);
                    let body = self
                        .cfg
                        .lock()
                        .unwrap_or_else(std::sync::PoisonError::into_inner)
                        .session_body
                        .clone();
                    respond(&mut out, 200, &body);
                }
                ("GET", "/permission") => {
                    let body = self
                        .cfg
                        .lock()
                        .unwrap_or_else(std::sync::PoisonError::into_inner)
                        .permission_body
                        .clone();
                    respond(&mut out, 200, &body);
                }
                ("GET", "/question") => {
                    let (body, status) = {
                        let cfg = self
                            .cfg
                            .lock()
                            .unwrap_or_else(std::sync::PoisonError::into_inner);
                        (cfg.question_body.clone(), cfg.question_status)
                    };
                    if status != 0 {
                        respond(&mut out, status, "");
                    } else {
                        respond(&mut out, 200, &body);
                    }
                }
                ("POST", p) if p.starts_with("/session/") => {
                    let (prompt, abort, perm, gate) = {
                        let cfg = self
                            .cfg
                            .lock()
                            .unwrap_or_else(std::sync::PoisonError::into_inner);
                        (
                            cfg.prompt_status,
                            cfg.abort_status,
                            cfg.permission_status,
                            cfg.before_mutation.clone(),
                        )
                    };
                    if let Some(gate) = gate {
                        gate(p);
                    }
                    let status = if p.ends_with("/prompt_async") {
                        if prompt != 0 {
                            prompt
                        } else {
                            204
                        }
                    } else if p.ends_with("/abort") {
                        if abort != 0 {
                            abort
                        } else {
                            200
                        }
                    } else if p.contains("/permissions/") {
                        if perm != 0 {
                            perm
                        } else {
                            200
                        }
                    } else {
                        404
                    };
                    respond(&mut out, status, if status == 200 { "true" } else { "" });
                }
                ("GET", p) if p.starts_with("/session/") && p.ends_with("/message") => {
                    let call = self.messages_hits.fetch_add(1, Ordering::SeqCst) + 1;
                    let (body, status, gate) = {
                        let cfg = self
                            .cfg
                            .lock()
                            .unwrap_or_else(std::sync::PoisonError::into_inner);
                        (
                            cfg.messages_body.clone(),
                            cfg.messages_status,
                            cfg.before_messages.clone(),
                        )
                    };
                    if let Some(gate) = gate {
                        // The gate can watch the CLIENT socket for disconnect
                        // (Go's r.Context()) via the SseConn wrapper.
                        let probe = SseConn {
                            stream: out.try_clone().expect("clone for gate"),
                        };
                        gate(call, &probe);
                    }
                    if status != 0 {
                        respond(&mut out, status, "");
                    } else {
                        respond(&mut out, 200, &body);
                    }
                }
                _ => respond(&mut out, 404, ""),
            }
        }
    }

    /// A tiny settor façade so tests read like the Go field assignments.
    pub(crate) struct FakeSetter<'a> {
        cfg: &'a mut FakeConfig,
    }

    impl FakeSetter<'_> {
        pub fn session_body(&mut self, s: &str) {
            self.cfg.session_body = s.to_string();
        }
        pub fn messages_body(&mut self, s: &str) {
            self.cfg.messages_body = s.to_string();
        }
        pub fn status_body(&mut self, s: &str) {
            self.cfg.status_body = s.to_string();
        }
        pub fn permission_body(&mut self, s: &str) {
            self.cfg.permission_body = s.to_string();
        }
        pub fn question_body(&mut self, s: &str) {
            self.cfg.question_body = s.to_string();
        }
        pub fn event_status(&mut self, s: u16) {
            self.cfg.event_status = s;
        }
        pub fn messages_status(&mut self, s: u16) {
            self.cfg.messages_status = s;
        }
        pub fn status_status(&mut self, s: u16) {
            self.cfg.status_status = s;
        }
        pub fn question_status(&mut self, s: u16) {
            self.cfg.question_status = s;
        }
        pub fn prompt_status(&mut self, s: u16) {
            self.cfg.prompt_status = s;
        }
        pub fn abort_status(&mut self, s: u16) {
            self.cfg.abort_status = s;
        }
        pub fn permission_status(&mut self, s: u16) {
            self.cfg.permission_status = s;
        }
        pub fn pin_guard(&mut self, s: &str) {
            self.cfg.pin_guard = s.to_string();
        }
    }

    fn respond(stream: &mut TcpStream, status: u16, body: &str) {
        let reason = match status {
            200 => "OK",
            204 => "No Content",
            401 => "Unauthorized",
            404 => "Not Found",
            500 => "Internal Server Error",
            _ => "Status",
        };
        let _ = stream.write_all(
            format!(
                "HTTP/1.1 {status} {reason}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                body.len()
            )
            .as_bytes(),
        );
    }

    pub(crate) const SSE_SERVER_CONNECTED: &str = r#"{"type":"server.connected","properties":{}}"#;

    // ---- the envelope shapes the transport tests fold (the Go tests inline
    // these; one builder each keeps every site on the same wire shape) ----

    /// One `permission.asked` for `sid`. `metadata` is a raw JSON object
    /// (`Some(r#"{"command":"ls -la"}"#)`) or None for the bare ask.
    pub(crate) fn permission_asked(sid: &str, id: &str, metadata: Option<&str>) -> String {
        let md = match metadata {
            Some(m) => format!(r#","metadata":{m}"#),
            None => String::new(),
        };
        format!(
            r#"{{"type":"permission.asked","properties":{{"id":"{id}","sessionID":"{sid}","permission":"bash","patterns":["ls"]{md}}}}}"#
        )
    }

    /// The matching `permission.replied` (`reply` is opencode's native
    /// vocabulary: once/always/reject).
    pub(crate) fn permission_replied(sid: &str, id: &str, reply: &str) -> String {
        format!(
            r#"{{"type":"permission.replied","properties":{{"sessionID":"{sid}","requestID":"{id}","reply":"{reply}"}}}}"#
        )
    }

    /// A live busy boundary for `sid`.
    pub(crate) fn session_status_busy(sid: &str) -> String {
        format!(
            r#"{{"type":"session.status","properties":{{"sessionID":"{sid}","status":{{"type":"busy"}}}}}}"#
        )
    }

    /// A live idle boundary for `sid`.
    pub(crate) fn session_idle(sid: &str) -> String {
        format!(r#"{{"type":"session.idle","properties":{{"sessionID":"{sid}"}}}}"#)
    }

    /// A `session.created` whose envelope sessionID and info.id are supplied
    /// separately, so a test can make them disagree (the malformed-pin case).
    pub(crate) fn session_created(session_id: &str, info_id: &str, dir: &str) -> String {
        format!(
            r#"{{"type":"session.created","properties":{{"sessionID":"{session_id}","info":{{"id":"{info_id}","directory":"{dir}","parentID":""}}}}}}"#
        )
    }
}

#[cfg(test)]
mod tests {
    use super::fake::{
        permission_asked, permission_replied, session_created, session_idle, session_status_busy,
        FakeOpencode, SSE_SERVER_CONNECTED,
    };
    use super::*;
    use std::sync::atomic::AtomicI64;

    /// The opencode session id + workdir the sanitized fixture was captured
    /// under (`ocFixtureSID`/`ocFixtureDir`). canonical_dir falls back to a
    /// lexical clean for a non-existent path, so both sides compare equal
    /// without the directory existing on the test host.
    const SID: &str = "ses_07cbd4370ffeF17Wb3Ius82a2g";
    const DIR: &str = "/private/tmp/oc-cap-kAr0";
    /// A SECOND session in the same fake's store — the global-store hazard
    /// made concrete (`ocOtherSID`).
    const OTHER_SID: &str = "ses_07cbd4370ffeOTHERsession9zz";
    const OTHER_DIR: &str = "/private/tmp/oc-cap-other";

    /// The GET /session/{id}/message body (`ocRESTMessages`): the same turn
    /// as the SSE fixture in REST {info,parts}[] shape, so a reconnect reseed
    /// dedups against the SSE arc.
    const OC_REST_MESSAGES: &str = r#"[{"info":{"id":"msg_f8342bca6001AF4ZtucXXAMSBG","sessionID":"ses_07cbd4370ffeF17Wb3Ius82a2g","role":"user","time":{"created":1784613616806},"summary":{"diffs":[]},"agent":"build","model":{"providerID":"zai-coding-plan","modelID":"glm-5.2"}},"parts":[{"id":"prt_f8342bcab001S05uV1SU0MDVyM","sessionID":"ses_07cbd4370ffeF17Wb3Ius82a2g","messageID":"msg_f8342bca6001AF4ZtucXXAMSBG","type":"text","text":"Use the bash tool to run ls in the current directory, then tell me how many .txt files there are. Be brief."}]},{"info":{"id":"msg_f8342bd01001y7c4BWOcifP2Va","sessionID":"ses_07cbd4370ffeF17Wb3Ius82a2g","role":"assistant","time":{"created":1784613616897,"completed":1784613621217},"parentID":"msg_f8342bca6001AF4ZtucXXAMSBG","modelID":"glm-5.2","providerID":"zai-coding-plan","mode":"build","agent":"build","path":{"cwd":"/private/tmp/oc-cap-kAr0","root":"/"},"cost":0,"tokens":{"total":7510,"input":7413,"output":11,"reasoning":22,"cache":{"read":64,"write":0}},"finish":"tool-calls"},"parts":[{"id":"prt_f8342cd9f00143bbhEOq5FsLv0","sessionID":"ses_07cbd4370ffeF17Wb3Ius82a2g","messageID":"msg_f8342bd01001y7c4BWOcifP2Va","type":"step-start"},{"id":"prt_f8342cda30012K99PZ0UzpgPGT","sessionID":"ses_07cbd4370ffeF17Wb3Ius82a2g","messageID":"msg_f8342bd01001y7c4BWOcifP2Va","type":"reasoning","text":"The user wants me to run ls in the current directory and tell them how many .txt files there are.","time":{"start":1784613621155,"end":1784613621161}},{"id":"prt_f8342cdab001rx5DsXIbsWW79j","sessionID":"ses_07cbd4370ffeF17Wb3Ius82a2g","messageID":"msg_f8342bd01001y7c4BWOcifP2Va","type":"tool","callID":"call_4c5b28f16dae4e6183bc6cf1","tool":"bash","state":{"status":"completed","input":{"command":"ls"},"output":"a.txt\nb.txt\nc.txt\n","title":"ls","metadata":{"output":"a.txt\nb.txt\nc.txt\n","exit":0,"truncated":false},"time":{"start":1784613621207,"end":1784613621211}}},{"id":"prt_f8342cdde001wVPhUpApFH7zvn","sessionID":"ses_07cbd4370ffeF17Wb3Ius82a2g","messageID":"msg_f8342bd01001y7c4BWOcifP2Va","type":"step-finish","reason":"tool-calls","cost":0,"tokens":{"total":7510,"input":7413,"output":11,"reasoning":22,"cache":{"read":64,"write":0}}}]},{"info":{"id":"msg_f8342cde3001cV5WzoZm92qWBg","sessionID":"ses_07cbd4370ffeF17Wb3Ius82a2g","role":"assistant","time":{"created":1784613621219,"completed":1784613627684},"parentID":"msg_f8342bca6001AF4ZtucXXAMSBG","modelID":"glm-5.2","providerID":"zai-coding-plan","mode":"build","agent":"build","path":{"cwd":"/private/tmp/oc-cap-kAr0","root":"/"},"cost":0,"tokens":{"total":7530,"input":99,"output":7,"reasoning":0,"cache":{"read":7424,"write":0}},"finish":"stop"},"parts":[{"id":"prt_f8342e71a001L2RZmCk3eMll71","sessionID":"ses_07cbd4370ffeF17Wb3Ius82a2g","messageID":"msg_f8342cde3001cV5WzoZm92qWBg","type":"step-start"},{"id":"prt_f8342e71f001dudzMRadO96Nju","sessionID":"ses_07cbd4370ffeF17Wb3Ius82a2g","messageID":"msg_f8342cde3001cV5WzoZm92qWBg","type":"text","text":"3 .txt files.","time":{"start":1784613627679,"end":1784613627681}},{"id":"prt_f8342e722001MPkFiT810m8dLb","sessionID":"ses_07cbd4370ffeF17Wb3Ius82a2g","messageID":"msg_f8342cde3001cV5WzoZm92qWBg","type":"step-finish","reason":"stop","cost":0,"tokens":{"total":7530,"input":99,"output":7,"reasoning":0,"cache":{"read":7424,"write":0}}}]}]"#;

    /// A manually-advanced clock behind the watcher's now_fn (`hubClock`).
    #[derive(Clone)]
    struct TestClock(Arc<Mutex<DateTime<Utc>>>);

    impl TestClock {
        fn new() -> TestClock {
            TestClock(Arc::new(Mutex::new(
                DateTime::from_timestamp(1_700_000_000, 0).expect("epoch"),
            )))
        }
        fn now(&self) -> DateTime<Utc> {
            *self.0.lock().unwrap()
        }
        fn advance(&self, d: Duration) {
            let mut t = self.0.lock().unwrap();
            *t += chrono::Duration::from_std(d).expect("in range");
        }
        fn now_fn(&self) -> Arc<dyn Fn() -> DateTime<Utc> + Send + Sync> {
            let c = self.clone();
            Arc::new(move || c.now())
        }
    }

    fn new_watcher(
        f: &FakeOpencode,
        workdir: &str,
        agent_id: &str,
        clk: &TestClock,
    ) -> Arc<OpencodeWatcher> {
        OpencodeWatcher::new(f.port(), workdir, agent_id, clk.now_fn(), None)
    }

    fn fixture_frames() -> Vec<String> {
        let path = format!(
            "{}/../fixtures/jsonl/opencode_turn.jsonl",
            env!("CARGO_MANIFEST_DIR")
        );
        std::fs::read_to_string(&path)
            .expect("fixture readable")
            .lines()
            .filter(|l| !l.trim().is_empty())
            .map(str::to_string)
            .collect()
    }

    /// `pollUntil`: every 5ms up to ~2s (panics on timeout).
    fn poll_until(msg: &str, mut cond: impl FnMut() -> bool) {
        let deadline = std::time::Instant::now() + Duration::from_secs(2);
        while std::time::Instant::now() < deadline {
            if cond() {
                return;
            }
            std::thread::sleep(Duration::from_millis(5));
        }
        panic!("timed out waiting for: {msg}");
    }

    /// `refreshUntil`: drives refresh + drains rows until cond holds.
    fn refresh_until(
        w: &OpencodeWatcher,
        clk: &TestClock,
        rows: Option<&mut Vec<FeedMessage>>,
        msg: &str,
        cond: impl Fn() -> bool,
    ) {
        let mut rows = rows;
        let deadline = std::time::Instant::now() + Duration::from_secs(2);
        while std::time::Instant::now() < deadline {
            w.refresh(clk.now());
            if let Some(rows) = rows.as_deref_mut() {
                rows.extend(w.drain_pending());
            }
            if cond() {
                return;
            }
            std::thread::sleep(Duration::from_millis(5));
        }
        panic!("timed out waiting for: {msg}");
    }

    fn activity_of(w: &OpencodeWatcher, clk: &TestClock) -> RcActivity {
        w.snapshot(clk.now()).0
    }

    struct Row {
        role: &'static str,
        typ: &'static str,
        text_prefix: &'static str,
        tool_name: &'static str,
        detail_has: &'static str,
    }

    fn assert_rows(got: &[FeedMessage], want: &[Row]) {
        assert_eq!(got.len(), want.len(), "rows: {got:?}");
        for (i, wnt) in want.iter().enumerate() {
            let m = &got[i];
            assert_eq!(
                (m.role.as_str(), m.typ.as_str()),
                (wnt.role, wnt.typ),
                "row {i}"
            );
            if !wnt.text_prefix.is_empty() {
                assert!(m.text.starts_with(wnt.text_prefix), "row {i}: {:?}", m.text);
            }
            if !wnt.tool_name.is_empty() {
                assert_eq!(
                    m.tool.as_ref().map(|t| t.name.as_str()),
                    Some(wnt.tool_name),
                    "row {i}"
                );
            }
            if !wnt.detail_has.is_empty() {
                assert!(
                    m.tool
                        .as_ref()
                        .is_some_and(|t| t.detail.contains(wnt.detail_has)),
                    "row {i}: {:?}",
                    m.tool
                );
            }
        }
    }

    use super::super::messages::{
        APPROVAL_DECISION_ALLOW, APPROVAL_DECISION_ALLOW_ALWAYS, APPROVAL_DECISION_DENY,
        APPROVAL_STATUS_PENDING, APPROVAL_STATUS_RESOLVED, FEED_ROLE_ASSISTANT, FEED_ROLE_TOOL,
        FEED_ROLE_USER, FEED_TYPE_APPROVAL_REQUEST, FEED_TYPE_REASONING, FEED_TYPE_STATUS,
        FEED_TYPE_TEXT, FEED_TYPE_TOOL_RESULT, FEED_TYPE_TOOL_USE,
    };
    use super::super::watch::merged_activity;

    const FIXTURE_ROWS: [Row; 5] = [
        Row {
            role: FEED_ROLE_USER,
            typ: FEED_TYPE_TEXT,
            text_prefix: "Use the bash tool",
            tool_name: "",
            detail_has: "",
        },
        Row {
            role: FEED_ROLE_ASSISTANT,
            typ: FEED_TYPE_REASONING,
            text_prefix: "The user wants",
            tool_name: "",
            detail_has: "",
        },
        Row {
            role: FEED_ROLE_TOOL,
            typ: FEED_TYPE_TOOL_USE,
            text_prefix: "",
            tool_name: "bash",
            detail_has: "ls",
        },
        Row {
            role: FEED_ROLE_TOOL,
            typ: FEED_TYPE_TOOL_RESULT,
            text_prefix: "",
            tool_name: "bash",
            detail_has: "a.txt",
        },
        Row {
            role: FEED_ROLE_ASSISTANT,
            typ: FEED_TYPE_TEXT,
            text_prefix: "3 .txt files.",
            tool_name: "",
            detail_has: "",
        },
    ];

    fn busy_status_body() -> String {
        format!(r#"{{"{SID}":{{"type":"busy"}}}}"#)
    }

    // Mirrors TestOpencodeWatcherPinFromSessionCreated.
    #[test]
    fn pin_from_session_created() {
        let f = FakeOpencode::new();
        f.set(|s| s.status_body(&busy_status_body()));
        let frames = fixture_frames();
        f.on_event(move |_conn, c| {
            c.write_sse(SSE_SERVER_CONNECTED);
            for fr in &frames {
                c.write_sse(fr);
            }
            c.hold_until_disconnect();
        });

        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, "", &clk);
        let mut rows = Vec::new();
        refresh_until(
            &w,
            &clk,
            Some(&mut rows),
            "activity settles to needs_input",
            || activity_of(&w, &clk) == RcActivity::NeedsInput,
        );

        let (act, msg, fresh, exp) = w.snapshot(clk.now());
        assert_eq!(act, RcActivity::NeedsInput);
        assert!(
            fresh && !exp,
            "settled+healthy: fresh={fresh} expired={exp}"
        );
        assert_eq!(msg, "3 .txt files.");
        assert_rows(&rows, &FIXTURE_ROWS);

        // The discovered id is back-write material exactly once.
        assert_eq!(w.drain_confirmed_agent_id(), SID);
        assert_eq!(w.drain_confirmed_agent_id(), "", "drained once");
        w.close();
    }

    // Mirrors TestOpencodeWatcherPinViaPriorAgentID.
    #[test]
    fn pin_via_prior_agent_id() {
        let f = FakeOpencode::new();
        f.set(|s| {
            s.messages_body(OC_REST_MESSAGES);
            s.status_body("{}"); // idle-omitted → synthesized idle → needs_input
        });
        f.hold_open_sse();

        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        let mut rows = Vec::new();
        refresh_until(
            &w,
            &clk,
            Some(&mut rows),
            "REST seed reconstructs needs_input",
            || activity_of(&w, &clk) == RcActivity::NeedsInput,
        );
        assert_eq!(rows.len(), 5, "seeded feed rows: {rows:?}");
        assert_eq!(
            w.drain_confirmed_agent_id(),
            "",
            "a prior back-write is not re-confirmed"
        );
        w.close();
    }

    // Mirrors TestOpencodeWatcherRESTCandidateFollowOnly.
    #[test]
    fn rest_candidate_follow_only() {
        let f = FakeOpencode::new();
        f.set(|s| {
            s.session_body(&format!(
                r#"[{{"id":"{SID}","directory":"{DIR}","parentID":""}}]"#
            ));
            s.messages_body(OC_REST_MESSAGES);
            s.status_body(&busy_status_body());
        });
        let release = Arc::new(AtomicBool::new(false));
        let rel = Arc::clone(&release);
        f.on_event(move |_conn, c| {
            c.write_sse(SSE_SERVER_CONNECTED);
            while !rel.load(Ordering::Relaxed) {
                if c.disconnected() {
                    return;
                }
                std::thread::sleep(Duration::from_millis(5));
            }
            c.write_sse(&session_status_busy(SID));
            c.hold_until_disconnect();
        });

        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, "", &clk);
        poll_until("candidate established via GET /session", || {
            f.session_hits.load(Ordering::SeqCst) >= 1
        });
        // Give the follow-only seed a moment; the id must stay undrained.
        for _ in 0..20 {
            w.refresh(clk.now());
            assert_eq!(
                w.drain_confirmed_agent_id(),
                "",
                "no back-write before SSE confirm"
            );
            std::thread::sleep(Duration::from_millis(5));
        }
        release.store(true, Ordering::Relaxed);
        poll_until("candidate confirmed by SSE evidence", || {
            w.drain_confirmed_agent_id() == SID
        });
        w.close();
    }

    // Mirrors TestOpencodeWatcherFiltersChildSibling.
    #[test]
    fn filters_child_sibling() {
        let f = FakeOpencode::new();
        f.set(|s| s.status_body(&busy_status_body()));
        f.on_event(move |_conn, c| {
            c.write_sse(SSE_SERVER_CONNECTED);
            c.write_sse(&session_created(SID, SID, DIR));
            c.write_sse(r#"{"type":"message.updated","properties":{"sessionID":"ses_SIBLING","info":{"id":"m_sib","role":"user","time":{"created":1784613616806}}}}"#);
            c.write_sse(r#"{"type":"message.part.updated","properties":{"sessionID":"ses_SIBLING","part":{"id":"p_sib","messageID":"m_sib","type":"text","text":"sibling prompt"}}}"#);
            c.write_sse(r#"{"type":"message.part.updated","properties":{"sessionID":"ses_CHILD","part":{"id":"p_child","messageID":"m_child","type":"text","text":"child text"}}}"#);
            c.write_sse(&format!(
                r#"{{"type":"message.updated","properties":{{"sessionID":"{SID}","info":{{"id":"m_root","role":"user","time":{{"created":1784613616806}}}}}}}}"#
            ));
            c.write_sse(&format!(
                r#"{{"type":"message.part.updated","properties":{{"sessionID":"{SID}","part":{{"id":"p_root","messageID":"m_root","type":"text","text":"root prompt"}}}}}}"#
            ));
            c.write_sse(&session_idle(SID));
            c.hold_until_disconnect();
        });

        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, "", &clk);
        let mut rows = Vec::new();
        refresh_until(&w, &clk, Some(&mut rows), "root session settles", || {
            activity_of(&w, &clk) == RcActivity::NeedsInput
        });
        assert_eq!(rows.len(), 1, "only the root session's prompt: {rows:?}");
        assert_eq!(rows[0].role, FEED_ROLE_USER);
        assert!(rows[0].text.starts_with("root prompt"));
        w.close();
    }

    // Mirrors TestOpencodeWatcherSeedBeforeSubscribe.
    #[test]
    fn seed_before_subscribe() {
        let f = FakeOpencode::new();
        f.set(|s| {
            s.messages_body(OC_REST_MESSAGES);
            s.status_body(&busy_status_body());
        });
        f.on_event(move |_conn, c| {
            c.write_sse(SSE_SERVER_CONNECTED);
            // Written immediately after server.connected — i.e. during the
            // REST-seed window. It must still be folded.
            c.write_sse(&session_idle(SID));
            c.hold_until_disconnect();
        });

        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        let mut rows = Vec::new();
        refresh_until(
            &w,
            &clk,
            Some(&mut rows),
            "the during-seed idle folds",
            || activity_of(&w, &clk) == RcActivity::NeedsInput,
        );
        assert_eq!(rows.len(), 5, "seed history intact: {rows:?}");
        w.close();
    }

    // Mirrors TestOpencodeWatcherIdleReseed.
    #[test]
    fn idle_reseed() {
        let f = FakeOpencode::new();
        f.set(|s| {
            s.messages_body(OC_REST_MESSAGES);
            s.status_body("{}");
        });
        f.hold_open_sse();
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        refresh_until(&w, &clk, None, "synthesized idle → needs_input", || {
            activity_of(&w, &clk) == RcActivity::NeedsInput
        });
        w.close();
    }

    // Mirrors TestOpencodeWatcherReconnectReseedIdempotent.
    #[test]
    fn reconnect_reseed_idempotent() {
        let f = FakeOpencode::new();
        f.set(|s| {
            s.status_body(&busy_status_body());
            s.messages_body(OC_REST_MESSAGES);
        });
        let frames = fixture_frames();
        f.on_event(move |conn, c| {
            c.write_sse(SSE_SERVER_CONNECTED);
            for fr in &frames {
                c.write_sse(fr);
            }
            if conn == 1 {
                return; // first connection ends → reconnect
            }
            c.hold_until_disconnect();
        });

        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, "", &clk);
        let mut rows = Vec::new();
        let deadline = std::time::Instant::now() + Duration::from_secs(2);
        while rows.len() < 5 {
            assert!(
                std::time::Instant::now() < deadline,
                "timed out waiting for the first connection's feed: {rows:?}"
            );
            w.refresh(clk.now());
            rows.extend(w.drain_pending());
            std::thread::sleep(Duration::from_millis(5));
        }
        assert_eq!(rows.len(), 5);
        poll_until("watcher reconnects", || {
            f.event_conns.load(Ordering::SeqCst) >= 2
        });
        for _ in 0..40 {
            w.refresh(clk.now());
            rows.extend(w.drain_pending());
            std::thread::sleep(Duration::from_millis(5));
        }
        assert_eq!(rows.len(), 5, "dedup idempotent: {rows:?}");
        w.close();
    }

    // Mirrors TestOpencodeWatcherStaleFallsToStability.
    #[test]
    fn stale_falls_to_stability() {
        let f = FakeOpencode::new();
        f.set(|s| s.status_body(&busy_status_body()));
        f.hold_open_sse(); // no further frames: last_frame_at freezes

        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        refresh_until(&w, &clk, None, "seed → working, healthy", || {
            let (act, _, fresh, _) = w.snapshot(clk.now());
            act == RcActivity::Working && fresh
        });

        clk.advance(OC_FRAME_STALE_WINDOW + Duration::from_secs(1));
        w.refresh(clk.now());
        let (act, _, fresh, exp) = w.snapshot(clk.now());
        assert_eq!(act, RcActivity::Working, "verdict retained, just untrusted");
        assert!(!fresh && !exp, "heartbeat-stale: both flags false");
        let (merged, _) = merged_activity(act, "", fresh, exp, RcActivity::Idle);
        assert_eq!(merged, RcActivity::Idle, "stability drives a stale watcher");
        w.close();
    }

    // Mirrors TestOpencodeWatcherCloseDuringBlockedRead.
    #[test]
    fn close_during_blocked_read() {
        let f = FakeOpencode::new();
        f.hold_open_sse();
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        poll_until("SSE connection established", || {
            f.event_conns.load(Ordering::SeqCst) >= 1
        });

        let w2 = Arc::clone(&w);
        let closer = std::thread::spawn(move || w2.close());
        assert!(
            closer.join().is_ok(),
            "close() must return (it is non-blocking)"
        );
        assert!(
            w.wait_done(Duration::from_secs(2)),
            "thread leaked (run did not exit after close)"
        );
        w.close(); // idempotent
    }

    // Mirrors TestOpencodeWatcherUnreachablePort.
    #[test]
    fn unreachable_port() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").expect("bind");
        let port = listener.local_addr().expect("addr").port();
        drop(listener);

        let clk = TestClock::new();
        let w = OpencodeWatcher::new(port, DIR, SID, clk.now_fn(), None);
        for _ in 0..20 {
            w.refresh(clk.now());
            let (_, _, fresh, exp) = w.snapshot(clk.now());
            assert!(!fresh && !exp, "unreachable watcher must have no authority");
            std::thread::sleep(Duration::from_millis(5));
        }
        w.close();
        assert!(
            w.wait_done(Duration::from_secs(2)),
            "thread leaked after close"
        );
    }

    // Mirrors TestOpencodeWatcher401.
    #[test]
    fn event_401_backs_off() {
        let f = FakeOpencode::new();
        f.set(|s| s.event_status(401));
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        poll_until("at least one 401 attempt", || {
            f.event_conns.load(Ordering::SeqCst) >= 1
        });
        std::thread::sleep(Duration::from_millis(300));
        let n = f.event_conns.load(Ordering::SeqCst);
        assert!(n <= 30, "401 hot-loop: {n} attempts in ~300ms");
        w.refresh(clk.now());
        let (_, _, fresh, _) = w.snapshot(clk.now());
        assert!(!fresh, "a 401 watcher must never report fresh");
        w.close();
    }

    // Mirrors TestOpencodeWatcherMalformedFrame.
    #[test]
    fn malformed_frame_tolerated() {
        let f = FakeOpencode::new();
        f.set(|s| s.status_body(&busy_status_body()));
        f.on_event(move |_conn, c| {
            c.write_sse(SSE_SERVER_CONNECTED);
            c.write_sse(&session_created(SID, SID, DIR));
            c.write_sse("this is not json at all");
            c.write_sse(r#"{"type":"#);
            c.write_sse(&session_idle(SID));
            c.hold_until_disconnect();
        });
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, "", &clk);
        refresh_until(&w, &clk, None, "arc folds despite garbage frames", || {
            activity_of(&w, &clk) == RcActivity::NeedsInput
        });
        w.close();
    }

    // Mirrors TestOpencodeWatcherOversizedFrame.
    #[test]
    fn oversized_frame_reconnects() {
        let f = FakeOpencode::new();
        f.on_event(move |conn, c| {
            if conn == 1 {
                c.write_raw("data: ");
                c.write_raw(&"x".repeat(MAX_SSE_LINE_BYTES + 1024));
                c.write_raw("\n\n");
                c.hold_until_disconnect();
                return;
            }
            c.write_sse(SSE_SERVER_CONNECTED);
            c.hold_until_disconnect();
        });
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        poll_until("watcher reconnects past the oversized frame", || {
            f.event_conns.load(Ordering::SeqCst) >= 2
        });
        w.refresh(clk.now()); // still alive, no panic
        w.close();
    }

    // ---- approvals over the live transport + the verb lane ----

    fn rt() -> tokio::runtime::Runtime {
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("test runtime")
    }

    /// `askThenReply`: one permission ask, its matching reply held until the
    /// flag flips.
    fn ask_then_reply(f: &FakeOpencode) -> Arc<AtomicBool> {
        f.set(|s| s.status_body(&busy_status_body()));
        let release = Arc::new(AtomicBool::new(false));
        let rel = Arc::clone(&release);
        let ask = permission_asked(SID, "per_1", Some(r#"{"command":"ls -la"}"#));
        let replied = permission_replied(SID, "per_1", "once");
        f.on_event(move |_conn, c| {
            c.write_sse(SSE_SERVER_CONNECTED);
            c.write_sse(&ask);
            while !rel.load(Ordering::Relaxed) {
                if c.disconnected() {
                    return;
                }
                std::thread::sleep(Duration::from_millis(5));
            }
            c.write_sse(&replied);
            c.hold_until_disconnect();
        });
        release
    }

    // Mirrors TestOpencodeWatcherApprovalArc.
    #[test]
    fn approval_arc() {
        let f = FakeOpencode::new();
        let release = ask_then_reply(&f);
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);

        let mut rows = Vec::new();
        refresh_until(
            &w,
            &clk,
            Some(&mut rows),
            "the ask blocks the session",
            || activity_of(&w, &clk) == RcActivity::NeedsApproval,
        );
        let (_, _, fresh, exp) = w.snapshot(clk.now());
        assert!(fresh && !exp, "needs_approval is settled while healthy");
        let pend = w.pending_approvals();
        assert_eq!(pend.len(), 1);
        assert_eq!(
            (
                pend[0].id.as_str(),
                pend[0].status.as_str(),
                pend[0].decisions.len()
            ),
            ("per_1", APPROVAL_STATUS_PENDING, 3)
        );
        assert_eq!(
            w.approval_state("per_1"),
            Some((APPROVAL_STATUS_PENDING.to_string(), String::new()))
        );
        assert_eq!(rows.len(), 1, "one pending approval row: {rows:?}");
        assert_eq!(rows[0].typ, FEED_TYPE_APPROVAL_REQUEST);
        assert_eq!(
            rows[0].tool.as_ref().map(|t| t.detail.as_str()),
            Some("ls -la"),
            "the command detail rides the row"
        );

        release.store(true, Ordering::Relaxed);
        refresh_until(
            &w,
            &clk,
            Some(&mut rows),
            "the reply releases the session",
            || activity_of(&w, &clk) == RcActivity::Working,
        );
        assert!(w.pending_approvals().is_empty());
        assert_eq!(
            w.approval_state("per_1"),
            Some((
                APPROVAL_STATUS_RESOLVED.to_string(),
                APPROVAL_DECISION_ALLOW.to_string()
            ))
        );
        assert_eq!(rows.len(), 2, "the resolved row followed: {rows:?}");
        let a = rows[1].approval.as_ref().expect("approval");
        assert_eq!(
            (a.status.as_str(), a.decision.as_str()),
            (APPROVAL_STATUS_RESOLVED, APPROVAL_DECISION_ALLOW)
        );
        w.close();
    }

    // Mirrors TestOpencodeWatcherNeedsApprovalDemotedOnDeadStream.
    #[test]
    fn needs_approval_demoted_on_dead_stream() {
        let f = FakeOpencode::new();
        f.set(|s| s.status_body(&busy_status_body()));
        f.stream_ask(SID, "per_1");
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        refresh_until(&w, &clk, None, "needs_approval, healthy", || {
            let (act, _, fresh, _) = w.snapshot(clk.now());
            act == RcActivity::NeedsApproval && fresh
        });

        clk.advance(OC_FRAME_STALE_WINDOW + Duration::from_secs(1));
        w.refresh(clk.now());
        let (act, _, fresh, exp) = w.snapshot(clk.now());
        assert_eq!(
            act,
            RcActivity::NeedsApproval,
            "verdict retained, just untrusted"
        );
        assert!(!fresh && !exp, "heartbeat-stale: both flags false");
        let (merged, _) = merged_activity(act, "", fresh, exp, RcActivity::Idle);
        assert_eq!(merged, RcActivity::Idle, "stability drives a dead stream");
        w.close();
    }

    // Mirrors TestOpencodeWatcherSeedRebuildsApprovals.
    #[test]
    fn seed_rebuilds_approvals() {
        let f = FakeOpencode::new();
        f.set(|s| {
            s.status_body(&busy_status_body());
            s.permission_body(&format!(
                r#"[{{"id":"per_seed","sessionID":"{SID}","permission":"bash","patterns":["ls"],"metadata":{{"command":"ls"}}}},{{"id":"per_other","sessionID":"ses_other","permission":"edit","patterns":["x.go"]}}]"#
            ));
            s.question_body(&format!(
                r#"[{{"id":"que_seed","sessionID":"{SID}","questions":[{{"header":"Which file?"}}]}}]"#
            ));
        });
        f.on_event(move |conn, c| {
            c.write_sse(SSE_SERVER_CONNECTED);
            if conn == 1 {
                return; // drop the first connection → reconnect + full reseed
            }
            c.hold_until_disconnect();
        });
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        let mut rows = Vec::new();
        refresh_until(
            &w,
            &clk,
            Some(&mut rows),
            "the seed rebuilds the open asks",
            || activity_of(&w, &clk) == RcActivity::NeedsApproval,
        );
        poll_until("a reconnect reseeds", || {
            f.event_conns.load(Ordering::SeqCst) >= 2
        });
        refresh_until(&w, &clk, Some(&mut rows), "the reseed is folded", || {
            f.messages_hits.load(Ordering::SeqCst) >= 2
        });
        w.refresh(clk.now());
        rows.extend(w.drain_pending());

        let pend = w.pending_approvals();
        assert_eq!(pend.len(), 1);
        assert_eq!(pend[0].id, "per_seed", "only the pinned session's ask");
        assert_eq!(
            w.approval_state("per_other"),
            None,
            "another session's permission must never enter this fold"
        );
        let approvals = rows
            .iter()
            .filter(|m| m.typ == FEED_TYPE_APPROVAL_REQUEST)
            .count();
        let statuses = rows.iter().filter(|m| m.typ == FEED_TYPE_STATUS).count();
        assert_eq!(
            (approvals, statuses),
            (1, 1),
            "one approval row + one question status row across both seeds: {rows:?}"
        );
        w.close();
    }

    // Mirrors TestOpencodeWatcherSeedRetiresAnsweredApproval.
    #[test]
    fn seed_retires_answered_approval() {
        let f = FakeOpencode::new();
        f.set(|s| {
            s.status_body(&busy_status_body());
            s.permission_body(&format!(
                r#"[{{"id":"per_1","sessionID":"{SID}","permission":"bash","patterns":["ls"]}}]"#
            ));
        });
        let drop_conn = Arc::new(AtomicBool::new(false));
        let d = Arc::clone(&drop_conn);
        f.on_event(move |conn, c| {
            c.write_sse(SSE_SERVER_CONNECTED);
            if conn == 1 {
                while !d.load(Ordering::Relaxed) {
                    if c.disconnected() {
                        return;
                    }
                    std::thread::sleep(Duration::from_millis(5));
                }
                return; // drop → reconnect + reseed
            }
            c.hold_until_disconnect();
        });
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        refresh_until(&w, &clk, None, "the seeded ask blocks the session", || {
            activity_of(&w, &clk) == RcActivity::NeedsApproval
        });

        // The operator answers in the TUI while our stream is down.
        f.set(|s| s.permission_body("[]"));
        drop_conn.store(true, Ordering::Relaxed);

        refresh_until(
            &w,
            &clk,
            None,
            "the reseed retires the answered ask",
            || activity_of(&w, &clk) == RcActivity::Working,
        );
        assert!(w.pending_approvals().is_empty());
        assert_eq!(
            w.approval_state("per_1"),
            Some((APPROVAL_STATUS_RESOLVED.to_string(), String::new())),
            "resolved with no decision — the TUI answered, the hub cannot know which way"
        );
        w.close();
    }

    // Mirrors TestOpencodeWatcherMarkApprovalResolved.
    #[test]
    fn mark_approval_resolved_semantics() {
        let f = FakeOpencode::new();
        let release = ask_then_reply(&f);
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        let mut rows = Vec::new();
        refresh_until(
            &w,
            &clk,
            Some(&mut rows),
            "the ask blocks the session",
            || activity_of(&w, &clk) == RcActivity::NeedsApproval,
        );

        assert!(w.mark_approval_resolved("per_1", APPROVAL_DECISION_ALLOW));
        // The verdict moves WITHOUT a refresh — the handler's answer must not
        // lag a tick.
        assert_eq!(activity_of(&w, &clk), RcActivity::Working);
        assert!(
            !w.mark_approval_resolved("per_1", APPROVAL_DECISION_ALLOW),
            "a same-decision replay reports false (no second POST)"
        );
        // An unseen id records a resolved tombstone.
        assert!(w.mark_approval_resolved("per_unknown", APPROVAL_DECISION_ALLOW));
        assert_eq!(
            w.approval_state("per_unknown").map(|(s, _)| s),
            Some(APPROVAL_STATUS_RESOLVED.to_string())
        );

        // opencode's own event for per_1 lands: still exactly one resolved
        // row FOR per_1.
        release.store(true, Ordering::Relaxed);
        poll_until("the replied frame is delivered", || {
            w.refresh(clk.now());
            rows.extend(w.drain_pending());
            rows.len() >= 2
        });
        w.refresh(clk.now());
        rows.extend(w.drain_pending());
        let resolved = rows
            .iter()
            .filter(|m| {
                m.approval
                    .as_ref()
                    .is_some_and(|a| a.status == APPROVAL_STATUS_RESOLVED && a.id == "per_1")
            })
            .count();
        assert_eq!(
            resolved, 1,
            "local mark + event are ONE resolution: {rows:?}"
        );
        w.close();
    }

    // Mirrors TestOpencodeWatcherApprovalsConcurrentAccess: the two-writer
    // discipline under real contention.
    #[test]
    fn approvals_concurrent_access() {
        const ASKS: usize = 20;
        let f = FakeOpencode::new();
        f.set(|s| s.status_body(&busy_status_body()));
        f.on_event(move |_conn, c| {
            c.write_sse(SSE_SERVER_CONNECTED);
            for i in 0..ASKS {
                c.write_sse(&permission_asked(SID, &format!("per_{i}"), None));
            }
            c.hold_until_disconnect();
        });
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        refresh_until(&w, &clk, None, "the asks are folded", || {
            w.pending_approvals().len() == ASKS
        });

        let resolved = Arc::new(AtomicI64::new(0));
        let mut handles = Vec::new();
        for _ in 0..4 {
            let w = Arc::clone(&w);
            let resolved = Arc::clone(&resolved);
            handles.push(std::thread::spawn(move || {
                for i in 0..ASKS {
                    let id = format!("per_{i}");
                    if w.mark_approval_resolved(&id, APPROVAL_DECISION_ALLOW) {
                        resolved.fetch_add(1, Ordering::SeqCst);
                    }
                    let _ = w.approval_state(&id);
                    let _ = w.pending_approvals();
                }
            }));
        }
        {
            let w = Arc::clone(&w);
            let clk = clk.clone();
            handles.push(std::thread::spawn(move || {
                for _ in 0..50 {
                    w.refresh(clk.now());
                    let _ = w.drain_pending();
                }
            }));
        }
        for h in handles {
            h.join().expect("thread");
        }
        assert_eq!(
            resolved.load(Ordering::SeqCst),
            ASKS as i64,
            "each ask resolves exactly once"
        );
        assert!(w.pending_approvals().is_empty());
        w.close();
    }

    // Mirrors TestOpencodeWatcherSeedHealsPermissionsWithQuestionReadFailing.
    #[test]
    fn seed_heals_permissions_with_question_read_failing() {
        let f = FakeOpencode::new();
        f.set(|s| {
            s.status_body(&busy_status_body());
            s.permission_body(&format!(
                r#"[{{"id":"per_1","sessionID":"{SID}","permission":"bash","patterns":["ls"]}}]"#
            ));
            s.question_status(500); // /question is down for the whole test
        });
        let drop_conn = Arc::new(AtomicBool::new(false));
        let d = Arc::clone(&drop_conn);
        f.on_event(move |conn, c| {
            c.write_sse(SSE_SERVER_CONNECTED);
            if conn == 1 {
                while !d.load(Ordering::Relaxed) {
                    if c.disconnected() {
                        return;
                    }
                    std::thread::sleep(Duration::from_millis(5));
                }
                return;
            }
            c.hold_until_disconnect();
        });
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        refresh_until(&w, &clk, None, "the seeded ask blocks the session", || {
            activity_of(&w, &clk) == RcActivity::NeedsApproval
        });
        f.set(|s| s.permission_body("[]"));
        drop_conn.store(true, Ordering::Relaxed);
        refresh_until(
            &w,
            &clk,
            None,
            "the reseed retires the answered ask",
            || activity_of(&w, &clk) == RcActivity::Working,
        );
        assert!(
            w.pending_approvals().is_empty(),
            "a failing /question must not block permission healing"
        );
        assert!(!w.has_open_approvals());
        w.close();
    }

    // ---- WS-B: the session-scoping invariant, adversarially ----

    fn pinned_verb_watcher(f: &FakeOpencode, sid: &str) -> (Arc<OpencodeWatcher>, TestClock) {
        f.set(|s| s.pin_guard(sid));
        f.hold_open_sse();
        let clk = TestClock::new();
        let w = new_watcher(f, DIR, sid, &clk);
        (w, clk)
    }

    // Mirrors TestOpencodeVerbsUseOnlyPinnedSessionRoutes.
    #[test]
    fn verbs_use_only_pinned_session_routes() {
        let f = FakeOpencode::new();
        let (w, _clk) = pinned_verb_watcher(&f, SID);
        let rt = rt();

        let turn_id = rt
            .block_on(w.start_turn("run the tests"))
            .expect("startTurn");
        assert!(
            turn_id.starts_with("oc-") && turn_id.len() > 3,
            "turn id = {turn_id:?}"
        );
        rt.block_on(w.interrupt_turn()).expect("interruptTurn");
        rt.block_on(w.resolve_approval("per_1", APPROVAL_DECISION_ALLOW))
            .expect("resolveApproval");

        let want = vec![
            format!("/session/{SID}/prompt_async"),
            format!("/session/{SID}/abort"),
            format!("/session/{SID}/permissions/per_1"),
        ];
        assert_eq!(f.post_paths(), want);
        assert_eq!(
            f.post_body("/prompt_async"),
            r#"{"parts":[{"type":"text","text":"run the tests"}]}"#
        );
        assert_eq!(f.post_body("/permissions/per_1"), r#"{"response":"once"}"#);
        assert!(f.violations().is_empty(), "{:?}", f.violations());
        w.close();
    }

    // Mirrors TestOpencodeResolveApprovalDecisionMapping.
    #[test]
    fn resolve_approval_decision_mapping() {
        let cases = [
            (APPROVAL_DECISION_ALLOW, "once"),
            (APPROVAL_DECISION_ALLOW_ALWAYS, "always"),
            (APPROVAL_DECISION_DENY, "reject"),
        ];
        for (decision, reply) in cases {
            let f = FakeOpencode::new();
            let (w, _clk) = pinned_verb_watcher(&f, SID);
            let rt = rt();
            rt.block_on(w.resolve_approval(&format!("per_{reply}"), decision))
                .expect("resolveApproval");
            assert_eq!(
                f.post_body(&format!("/permissions/per_{reply}")),
                format!(r#"{{"response":"{reply}"}}"#)
            );
            assert!(f.violations().is_empty());
            w.close();
        }
        // An out-of-enum decision never reaches the wire.
        let f = FakeOpencode::new();
        let (w, _clk) = pinned_verb_watcher(&f, SID);
        let rt = rt();
        assert!(rt.block_on(w.resolve_approval("per_1", "maybe")).is_err());
        assert!(
            f.post_paths().is_empty(),
            "an unmapped decision must not POST"
        );
        w.close();
    }

    // Mirrors TestOpencodeVerbsLeaveOtherSessionUntouched: two rc sessions
    // pinned to two opencode sessions in the SAME store.
    #[test]
    fn verbs_leave_other_session_untouched() {
        let f = FakeOpencode::new();
        f.set(|s| {
            s.session_body(&format!(
                r#"[{{"id":"{SID}","directory":"{DIR}","parentID":""}},{{"id":"{OTHER_SID}","directory":"{OTHER_DIR}","parentID":""}}]"#
            ));
        });
        f.hold_open_sse();
        let clk = TestClock::new();
        let a = new_watcher(&f, DIR, SID, &clk);
        let b = new_watcher(&f, OTHER_DIR, OTHER_SID, &clk);
        let rt = rt();
        rt.block_on(a.start_turn("steer A")).expect("startTurn");
        rt.block_on(a.interrupt_turn()).expect("interruptTurn");
        rt.block_on(a.resolve_approval("per_a", APPROVAL_DECISION_DENY))
            .expect("resolveApproval");

        let posts = f.post_paths();
        for p in &posts {
            assert!(
                p.starts_with(&format!("/session/{SID}/")),
                "verb on A touched {p}"
            );
        }
        assert_eq!(posts.len(), 3, "exactly the 3 A-scoped verb calls");
        a.close();
        b.close();
    }

    // Mirrors TestOpencodeVerbsUnpinnedRejectWithoutRequest.
    #[test]
    fn verbs_unpinned_reject_without_request() {
        let f = FakeOpencode::new(); // empty store: no candidate, so the pin stays ""
        let (w, _clk) = pinned_verb_watcher(&f, "");
        let rt = rt();
        assert_eq!(
            rt.block_on(w.start_turn("hi")),
            Err(OcWatcherError::NoAgentSession)
        );
        assert_eq!(
            rt.block_on(w.interrupt_turn()),
            Err(OcWatcherError::NoAgentSession)
        );
        assert_eq!(
            rt.block_on(w.resolve_approval("per_1", APPROVAL_DECISION_ALLOW)),
            Err(OcWatcherError::NoAgentSession)
        );
        assert!(f.post_paths().is_empty(), "no request at all");
        w.close();
    }

    // Mirrors TestOpencodeVerbsOnClosedWatcher.
    #[test]
    fn verbs_on_closed_watcher() {
        let f = FakeOpencode::new();
        let (w, _clk) = pinned_verb_watcher(&f, SID);
        w.close();
        let rt = rt();
        assert_eq!(rt.block_on(w.start_turn("hi")), Err(OcWatcherError::Closed));
        assert_eq!(rt.block_on(w.interrupt_turn()), Err(OcWatcherError::Closed));
        assert!(
            f.post_paths().is_empty(),
            "closed-watcher verbs send nothing"
        );
    }

    // Mirrors TestOpencodeVerbsUpstreamFailure.
    #[test]
    fn verbs_upstream_failure() {
        let f = FakeOpencode::new();
        f.set(|s| {
            s.prompt_status(500);
            s.abort_status(500);
            s.permission_status(500);
        });
        let (w, _clk) = pinned_verb_watcher(&f, SID);
        let rt = rt();
        assert!(rt.block_on(w.start_turn("hi")).is_err(), "500 must surface");
        assert!(rt.block_on(w.interrupt_turn()).is_err());
        assert!(rt
            .block_on(w.resolve_approval("per_1", APPROVAL_DECISION_ALLOW))
            .is_err());
        w.close();
    }

    // Mirrors TestOpencodeInterruptIdleSessionSucceeds.
    #[test]
    fn interrupt_idle_session_succeeds() {
        let f = FakeOpencode::new();
        f.set(|s| s.status_body("{}")); // idle
        let (w, clk) = pinned_verb_watcher(&f, SID);
        refresh_until(&w, &clk, None, "the session settles idle", || {
            activity_of(&w, &clk) == RcActivity::NeedsInput
        });
        let rt = rt();
        rt.block_on(w.interrupt_turn())
            .expect("interrupt on an idle session passes through");
        assert_eq!(f.post_paths(), vec![format!("/session/{SID}/abort")]);
        assert!(f.violations().is_empty());
        w.close();
    }

    // Mirrors TestOpencodeSeedGETsArePinFiltered.
    #[test]
    fn seed_gets_are_pin_filtered() {
        let f = FakeOpencode::new();
        f.set(|s| {
            s.status_body(&busy_status_body());
            s.permission_body(&format!(
                r#"[{{"id":"per_mine","sessionID":"{SID}","permission":"bash","patterns":["ls"]}},{{"id":"per_theirs","sessionID":"{OTHER_SID}","permission":"edit","patterns":["x.go"]}}]"#
            ));
            s.question_body(&format!(
                r#"[{{"id":"que_theirs","sessionID":"{OTHER_SID}","questions":[{{"header":"Which file?"}}]}}]"#
            ));
        });
        let (w, clk) = pinned_verb_watcher(&f, SID);
        refresh_until(
            &w,
            &clk,
            None,
            "the seed folds the pinned session's ask",
            || activity_of(&w, &clk) == RcActivity::NeedsApproval,
        );

        let pend = w.pending_approvals();
        assert_eq!(pend.len(), 1);
        assert_eq!(pend[0].id, "per_mine", "only the pinned session's ask");
        assert_eq!(w.approval_state("per_theirs"), None, "pin-filtered");
        assert!(w.has_open_approvals());

        let gets = f.get_paths();
        let mut saw_global = false;
        for p in &gets {
            if p == "/permission" || p == "/question" || p == "/session/status" {
                saw_global = true;
                continue;
            }
            if p.starts_with("/session/") {
                assert!(
                    p.starts_with(&format!("/session/{SID}/")),
                    "session-scoped GET {p} addresses a session other than the pin"
                );
            }
        }
        assert!(
            saw_global,
            "the seed uses the global discovery GETs: {gets:?}"
        );
        w.close();
    }

    // ---- pin hardening ----

    /// The pin shapes `valid_opencode_session_id` must reject (`ocBadPins`) —
    /// traversal, extra segments, query/fragment, whitespace, pre-encoded
    /// separators, and one over the 256-char cap.
    fn malformed_pins() -> Vec<String> {
        vec![
            "ses_A/../../session/VICTIM".to_string(),
            "ses_A/x".to_string(),
            "ses_A?scope=project".to_string(),
            "ses_A#frag".to_string(),
            "ses A".to_string(),
            "ses_A%2fVICTIM".to_string(),
            "../ses_A".to_string(),
            "s".repeat(300),
        ]
    }

    // Mirrors TestOpencodeWatcherRejectsMalformedPriorPin.
    #[test]
    fn rejects_malformed_prior_pin() {
        for bad in malformed_pins() {
            let f = FakeOpencode::new();
            f.hold_open_sse();
            let clk = TestClock::new();
            let w = new_watcher(&f, DIR, &bad, &clk);
            assert_eq!(w.get_pinned(), "", "a malformed pin is no pin ({bad:?})");
            let rt = rt();
            assert_eq!(
                rt.block_on(w.start_turn("hi")),
                Err(OcWatcherError::NoAgentSession),
                "{bad:?}"
            );
            assert_eq!(
                rt.block_on(w.resolve_approval("per_1", APPROVAL_DECISION_ALLOW)),
                Err(OcWatcherError::NoAgentSession),
                "{bad:?}"
            );
            assert!(f.post_paths().is_empty(), "{bad:?} produced requests");
            w.close();
        }
    }

    // Mirrors TestOpencodeWatcherRejectsMalformedDiscoveredPin.
    #[test]
    fn rejects_malformed_discovered_pin() {
        let f = FakeOpencode::new();
        f.set(|s| s.status_body(&busy_status_body()));
        let release = Arc::new(AtomicBool::new(false));
        let rel = Arc::clone(&release);
        f.on_event(move |_conn, c| {
            c.write_sse(SSE_SERVER_CONNECTED);
            c.write_sse(&session_created("evil", "ses_A/../../session/VICTIM", DIR));
            while !rel.load(Ordering::Relaxed) {
                if c.disconnected() {
                    return;
                }
                std::thread::sleep(Duration::from_millis(5));
            }
            c.write_sse(&session_created(SID, SID, DIR));
            c.hold_until_disconnect();
        });
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, "", &clk);
        for _ in 0..20 {
            w.refresh(clk.now());
            assert_eq!(
                w.get_pinned(),
                "",
                "a malformed session.created must not pin"
            );
            assert_eq!(w.drain_confirmed_agent_id(), "", "never back-written");
            std::thread::sleep(Duration::from_millis(5));
        }
        release.store(true, Ordering::Relaxed);
        poll_until("the well-formed id still pins", || w.get_pinned() == SID);
        w.close();
    }

    // Mirrors TestOpencodeSessionPathEscapesEverySegment.
    #[test]
    fn session_path_escapes_every_segment() {
        assert_eq!(
            oc_session_path("ses_A/../victim", &["permissions", "per/../x"]),
            "/session/ses_A%2F..%2Fvictim/permissions/per%2F..%2Fx"
        );
        assert_eq!(
            oc_session_path(SID, &["prompt_async"]),
            format!("/session/{SID}/prompt_async")
        );
        assert_eq!(
            oc_session_path(SID, &["permissions", "per_01HQ8Z3K.tool:2"]),
            format!("/session/{SID}/permissions/per_01HQ8Z3K.tool:2")
        );
    }

    // Pins path_escape to url.PathEscape's exact keep-set: the RFC-2396 marks
    // and `,`/`;` ARE escaped by Go (H8 review MEDIUM — the port kept 7 bytes
    // Go escapes; verified against a 0-255 Go oracle sweep).
    #[test]
    fn path_escape_matches_go_keep_set() {
        assert_eq!(
            oc_session_path("id,with;chars", &["per!1"]),
            "/session/id%2Cwith%3Bchars/per%211"
        );
        assert_eq!(
            oc_session_path("a!'()*b", &[]),
            "/session/a%21%27%28%29%2Ab"
        );
        // The bytes Go keeps stay kept.
        assert_eq!(
            oc_session_path("Az09-._~$&+=:@", &[]),
            "/session/Az09-._~$&+=:@"
        );
    }

    // Pins the fake's WS-B guard against GLOBAL mutation routes: a POST that
    // is not even /session/<a>/<b> is a violation, exactly like Go's regex
    // non-match (H8 review HIGH — the `?` early-returns silently passed
    // /permission/{id}/reply, THE canonical violation).
    #[test]
    fn mutation_violation_flags_global_routes() {
        for path in [
            "/permission/p1/reply",
            "/question/q1/reply",
            "/question/q1/reject",
            "/session/x",
            "/session/",
            "/session",
            "/",
            "//session/x/abort",
            "/SESSION/x/abort",
        ] {
            let v = fake::mutation_violation(path, "");
            assert!(
                v.as_deref()
                    .is_some_and(|m| m.contains("not a session-scoped mutation route")),
                "{path:?} must violate, got {v:?}"
            );
        }
        for path in [
            "/session/x/prompt_async",
            "/session/x/abort",
            "/session/x/permissions/p1",
        ] {
            assert_eq!(fake::mutation_violation(path, ""), None, "{path:?}");
        }
        assert!(
            fake::mutation_violation("/session/other/abort", "pinned")
                .is_some_and(|m| m.contains("not the pinned")),
            "pin mismatch still reported"
        );
    }

    // Pins the SSE line cap to bufio.Scanner's exact ErrTooLong boundary
    // (H8 review MEDIUM: the cap was enforced one 4 KiB read-chunk late, and
    // a complete over-cap line already buffered slipped through entirely).
    #[test]
    fn sse_line_cap_is_exact() {
        let frame = |line_len: usize| {
            let mut data = vec![b'x'; line_len];
            data.push(b'\n');
            data
        };
        let mut ok = SseScanner::new(
            std::io::Cursor::new(frame(MAX_SSE_LINE_BYTES - 1)),
            Box::new(|| {}),
        );
        assert!(
            ok.next_line().expect("cap-1 accepted").is_some(),
            "a line one under the cap scans"
        );
        let mut too_long = SseScanner::new(
            std::io::Cursor::new(frame(MAX_SSE_LINE_BYTES)),
            Box::new(|| {}),
        );
        assert!(
            too_long.next_line().is_err(),
            "a line of exactly the cap errors, as Go's Scanner does"
        );
    }

    // Pins canonical_dir's relativity preservation (H8 review LOW):
    // filepath.EvalSymlinks keeps a relative input relative, so a relative
    // event directory must not absolutize into a spurious pin match.
    #[test]
    fn canonical_dir_preserves_relativity() {
        assert_eq!(canonical_dir("."), ".");
        // "src" exists relative to the crate root cargo test runs from.
        let got = canonical_dir("src");
        assert_eq!(got, "src", "a relative existing dir stays relative");
        // Absolute inputs stay absolute (and resolved).
        assert!(canonical_dir("/").starts_with('/'));
    }

    // Mirrors TestValidOpencodeSessionID.
    #[test]
    fn valid_opencode_session_id_rules() {
        for ok in [SID, "ses_07cbd4370ffeF17Wb3Ius82a2g", "abc-123_XYZ", "9"] {
            assert!(valid_opencode_session_id(ok), "{ok:?}");
        }
        let mut bad: Vec<String> = vec![
            "".into(),
            "ses.A".into(),
            "ses:A".into(),
            "ses/A".into(),
            ".".into(),
        ];
        bad.extend(malformed_pins());
        for b in bad {
            assert!(!valid_opencode_session_id(&b), "{b:?}");
        }
    }

    // Mirrors TestOpencodeApprovalClaimSerializesConcurrentResolves.
    #[test]
    fn approval_claim_serializes_concurrent_resolves() {
        let f = FakeOpencode::new();
        let entered = Arc::new(AtomicBool::new(false));
        let release = Arc::new(AtomicBool::new(false));
        let (e, r) = (Arc::clone(&entered), Arc::clone(&release));
        f.before_mutation(move |_path| {
            e.store(true, Ordering::Relaxed);
            while !r.load(Ordering::Relaxed) {
                std::thread::sleep(Duration::from_millis(5));
            }
        });
        let (w, _clk) = pinned_verb_watcher(&f, SID);
        // Fold one pending ask directly so the claim has something to take
        // (Go reaches into w.fold the same way).
        w.fold_apply_for_test(permission_asked(SID, "per_1", None).as_bytes());

        assert_eq!(
            w.claim_approval("per_1", APPROVAL_DECISION_ALLOW),
            ApprovalClaim::Claimed
        );
        let w2 = Arc::clone(&w);
        let poster = std::thread::spawn(move || {
            rt().block_on(w2.resolve_approval("per_1", APPROVAL_DECISION_ALLOW))
        });
        poll_until("the first POST is in flight", || {
            entered.load(Ordering::Relaxed)
        });

        // While the first resolve is in flight, a second request cannot claim.
        assert_eq!(
            w.claim_approval("per_1", APPROVAL_DECISION_ALLOW),
            ApprovalClaim::Busy,
            "same decision included"
        );
        assert_eq!(
            w.claim_approval("per_1", APPROVAL_DECISION_DENY),
            ApprovalClaim::Busy
        );

        release.store(true, Ordering::Relaxed);
        poster.join().expect("thread").expect("resolveApproval");
        assert_eq!(
            w.commit_approval("per_1", APPROVAL_DECISION_ALLOW),
            APPROVAL_DECISION_ALLOW
        );
        // Post-commit the ask is settled (the claim was consumed).
        assert_eq!(
            w.claim_approval("per_1", APPROVAL_DECISION_ALLOW),
            ApprovalClaim::Settled
        );
        assert_eq!(f.post_paths().len(), 1, "exactly one upstream POST");
        assert!(f.violations().is_empty());
        w.close();
    }

    // Mirrors TestOpencodeApprovalClaimReleasedOnFailure.
    #[test]
    fn approval_claim_released_on_failure() {
        let f = FakeOpencode::new();
        let (w, _clk) = pinned_verb_watcher(&f, SID);
        w.fold_apply_for_test(permission_asked(SID, "per_1", None).as_bytes());
        assert_eq!(
            w.claim_approval("per_1", APPROVAL_DECISION_ALLOW),
            ApprovalClaim::Claimed
        );
        w.release_approval("per_1");
        assert_eq!(
            w.claim_approval("per_1", APPROVAL_DECISION_DENY),
            ApprovalClaim::Claimed,
            "a released claim is takeable again"
        );
        assert_eq!(
            w.approval_state("per_1").map(|(s, _)| s),
            Some(APPROVAL_STATUS_PENDING.to_string()),
            "still pending after a released claim"
        );
        w.close();
    }

    // Mirrors TestOpencodeCommitApprovalReportsTheRecordedDecision.
    #[test]
    fn commit_approval_reports_the_recorded_decision() {
        let f = FakeOpencode::new();
        let (w, _clk) = pinned_verb_watcher(&f, SID);
        w.fold_apply_for_test(permission_asked(SID, "per_1", None).as_bytes());
        // The stream's reply lands first, recording allow_always.
        w.fold_apply_for_test(permission_replied(SID, "per_1", "always").as_bytes());
        assert_eq!(
            w.commit_approval("per_1", APPROVAL_DECISION_ALLOW),
            APPROVAL_DECISION_ALLOW_ALWAYS,
            "the stream-recorded decision wins"
        );
        w.close();
    }

    // Mirrors TestOpencodeWatcherStaleSeedCompleteIgnored (fix #2): a
    // superseded connection's seedComplete must not authorize a newer
    // connection.
    #[test]
    fn stale_seed_complete_ignored() {
        let f = FakeOpencode::new();
        f.set(|s| {
            s.messages_body(OC_REST_MESSAGES);
            s.status_body(&busy_status_body());
        });
        let gate = Arc::new(AtomicBool::new(false));
        let g = Arc::clone(&gate);
        // Block connection B's seed (the 2nd /message call) until released,
        // so B is "seeding but not complete" while the test refreshes.
        f.before_messages(move |call, conn| {
            if call >= 2 {
                while !g.load(Ordering::Relaxed) && !conn.disconnected() {
                    std::thread::sleep(Duration::from_millis(5));
                }
            }
        });
        f.on_event(move |conn, c| {
            c.write_sse(SSE_SERVER_CONNECTED);
            if conn == 1 {
                return; // A: seeds, then the /event connection ENDS → reconnect to B
            }
            c.hold_until_disconnect(); // B: stays connected while its seed is gated
        });

        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        // messagesHits>=2 guarantees B's connect has advanced its generation
        // (begin_generation runs before the GET) — so A's queued seedComplete
        // is stale.
        poll_until("connection B begins seeding", || {
            f.messages_hits.load(Ordering::SeqCst) >= 2
        });
        for _ in 0..20 {
            w.refresh(clk.now());
            let (_, _, fresh, _) = w.snapshot(clk.now());
            assert!(
                !fresh,
                "a stale-generation seedComplete authorized the watcher"
            );
            std::thread::sleep(Duration::from_millis(5));
        }
        gate.store(true, Ordering::Relaxed);
        refresh_until(
            &w,
            &clk,
            None,
            "B's own seed completes → authoritative",
            || w.snapshot(clk.now()).2,
        );
        w.close();
    }

    // Mirrors TestOpencodeWatcherLiveStatusWinsOverRESTFallback (fix #3).
    #[test]
    fn live_status_wins_over_rest_fallback() {
        let f = FakeOpencode::new();
        f.set(|s| {
            s.messages_body("[]");
            s.status_body("{}"); // REST status: idle
        });
        f.on_event(move |_conn, c| {
            c.write_sse(SSE_SERVER_CONNECTED);
            // A live busy status buffered during the seed: live is authoritative.
            c.write_sse(&session_status_busy(SID));
            c.hold_until_disconnect();
        });
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        refresh_until(&w, &clk, None, "live busy wins over REST idle", || {
            activity_of(&w, &clk) == RcActivity::Working
        });
        for _ in 0..20 {
            w.refresh(clk.now());
            assert_eq!(
                activity_of(&w, &clk),
                RcActivity::Working,
                "the REST idle fallback must never flip a live-busy session"
            );
            std::thread::sleep(Duration::from_millis(5));
        }
        w.close();
    }

    // Mirrors TestOpencodeWatcherRESTIdleFallbackWhenNoLiveStatus.
    #[test]
    fn rest_idle_fallback_when_no_live_status() {
        let f = FakeOpencode::new();
        f.set(|s| {
            s.messages_body("[]");
            s.status_body("{}");
        });
        f.hold_open_sse();
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        refresh_until(&w, &clk, None, "REST idle fallback → needs_input", || {
            let (act, _, fresh, _) = w.snapshot(clk.now());
            act == RcActivity::NeedsInput && fresh
        });
        w.close();
    }

    // Mirrors TestOpencodeWatcherStatusSeedFailureReconnects (fix #5).
    #[test]
    fn status_seed_failure_reconnects() {
        let f = FakeOpencode::new();
        f.set(|s| {
            s.messages_body(OC_REST_MESSAGES);
            s.status_status(500);
        });
        f.hold_open_sse();
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        poll_until("status-seed failure forces reconnect", || {
            f.event_conns.load(Ordering::SeqCst) >= 2
        });
        for _ in 0..30 {
            w.refresh(clk.now());
            let (_, _, fresh, exp) = w.snapshot(clk.now());
            assert!(!fresh && !exp, "a failed status seed must not authorize");
            std::thread::sleep(Duration::from_millis(5));
        }
        w.close();
    }

    // Mirrors TestOpencodeWatcherCandidateSeedFailureReconnects (fix #4).
    #[test]
    fn candidate_seed_failure_reconnects() {
        let f = FakeOpencode::new();
        f.set(|s| {
            s.session_body(&format!(
                r#"[{{"id":"{SID}","directory":"{DIR}","parentID":""}}]"#
            ));
            s.messages_status(500); // the candidate seed's message fetch fails
        });
        f.hold_open_sse();
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, "", &clk); // no prior id → candidate path
        poll_until("candidate seed failure forces reconnect", || {
            f.event_conns.load(Ordering::SeqCst) >= 2
        });
        for _ in 0..30 {
            w.refresh(clk.now());
            let (_, _, fresh, exp) = w.snapshot(clk.now());
            assert!(!fresh && !exp);
            assert_eq!(
                w.drain_confirmed_agent_id(),
                "",
                "a failed candidate seed must never back-write"
            );
            std::thread::sleep(Duration::from_millis(5));
        }
        w.close();
    }

    // Mirrors TestOpencodeWatcherClosedNotFresh (fix #6).
    #[test]
    fn closed_not_fresh() {
        let f = FakeOpencode::new();
        f.set(|s| s.status_body(&busy_status_body()));
        f.hold_open_sse();
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        refresh_until(&w, &clk, None, "seed → working, fresh", || {
            let (act, _, fresh, _) = w.snapshot(clk.now());
            act == RcActivity::Working && fresh
        });
        w.close();
        w.refresh(clk.now()); // a post-close refresh no-ops
        let (_, _, fresh, exp) = w.snapshot(clk.now());
        assert!(!fresh && !exp, "a closed watcher has no authority");
        assert!(
            w.wait_done(Duration::from_secs(2)),
            "thread leaked after close"
        );
    }

    // Mirrors TestOpencodeWatcherOverflowRevokesAuthorityImmediately (fix #7).
    #[test]
    fn overflow_revokes_authority_immediately() {
        let f = FakeOpencode::new();
        f.set(|s| s.status_body(&busy_status_body()));
        let flood = Arc::new(AtomicBool::new(false));
        let fl = Arc::clone(&flood);
        f.on_event(move |conn, c| {
            if conn == 1 {
                c.write_sse(SSE_SERVER_CONNECTED);
                while !fl.load(Ordering::Relaxed) {
                    if c.disconnected() {
                        return;
                    }
                    std::thread::sleep(Duration::from_millis(5));
                }
                let frame = session_status_busy(SID);
                for _ in 0..(MAX_INBOX_ITEMS + 200) {
                    c.write_sse(&frame);
                }
                c.hold_until_disconnect();
                return;
            }
            // The forced reconnect stays SILENT (no server.connected → no
            // reseed) so the watcher must remain non-authoritative.
            c.hold_until_disconnect();
        });
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        refresh_until(&w, &clk, None, "seed → working, fresh", || {
            let (act, _, fresh, _) = w.snapshot(clk.now());
            act == RcActivity::Working && fresh
        });
        flood.store(true, Ordering::Relaxed);
        poll_until("overflow forces a reconnect", || {
            f.event_conns.load(Ordering::SeqCst) >= 2
        });
        for _ in 0..40 {
            w.refresh(clk.now());
            let (_, _, fresh, exp) = w.snapshot(clk.now());
            assert!(!fresh && !exp, "authority must stay revoked post-overflow");
            std::thread::sleep(Duration::from_millis(5));
        }
        w.close();
    }

    // Mirrors TestNextReconnectBackoff (fix #8, pure).
    #[test]
    fn next_reconnect_backoff_rules() {
        assert_eq!(
            next_reconnect_backoff(Duration::from_secs(2), true),
            OC_BACKOFF_BASE,
            "a successful seed resets to the floor"
        );
        assert!(
            next_reconnect_backoff(OC_BACKOFF_BASE, false) > OC_BACKOFF_BASE,
            "a post-connect failure must GROW the backoff"
        );
        let (mut cur, mut prev) = (OC_BACKOFF_BASE, OC_BACKOFF_BASE);
        for _ in 0..20 {
            cur = next_reconnect_backoff(cur, false);
            assert!(cur >= prev, "backoff must never shrink on a failure");
            prev = cur;
        }
        assert_eq!(cur, OC_BACKOFF_MAX, "repeated failures reach the cap");
    }

    // Mirrors TestOpencodeWatcherSeedFailGrowsBackoff (fix #8, live).
    #[test]
    fn seed_fail_grows_backoff() {
        let f = FakeOpencode::new();
        f.set(|s| s.messages_status(500));
        f.on_event(|_conn, c| {
            c.write_sse(SSE_SERVER_CONNECTED); // connected, but the seed 500s
        });
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        poll_until("multiple connect-then-seed-fail attempts", || {
            f.event_conns.load(Ordering::SeqCst) >= 2
        });
        poll_until("backoff grows beyond the floor", || {
            w.get_backoff() > OC_BACKOFF_BASE
        });
        w.close();
    }

    // Mirrors TestOpencodeWatcherHeartbeatKeepsFresh (fix #9): a stream that
    // delivers ONLY comment heartbeats must not go heartbeat-stale.
    #[test]
    fn heartbeat_keeps_fresh() {
        let f = FakeOpencode::new();
        f.set(|s| s.status_body(&busy_status_body()));
        let beat = Arc::new(AtomicI64::new(0));
        let b = Arc::clone(&beat);
        f.on_event(move |_conn, c| {
            c.write_sse(SSE_SERVER_CONNECTED);
            let mut sent = 0;
            loop {
                if c.disconnected() {
                    return;
                }
                let want = b.load(Ordering::Relaxed);
                if want > sent {
                    c.write_raw(": heartbeat\n\n"); // comment-only frame, no data:
                    sent = want;
                }
                std::thread::sleep(Duration::from_millis(5));
            }
        });
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        refresh_until(&w, &clk, None, "seed → working, fresh", || {
            let (act, _, fresh, _) = w.snapshot(clk.now());
            act == RcActivity::Working && fresh
        });

        // Advance to just under the stale window, then deliver a comment
        // heartbeat.
        clk.advance(OC_FRAME_STALE_WINDOW - Duration::from_secs(1));
        let target = clk.now();
        beat.store(1, Ordering::Relaxed);
        poll_until("comment heartbeat bumps last_frame_at", || {
            w.last_frame_at().is_some_and(|t| t >= target)
        });

        // Past the ORIGINAL stale window but within the heartbeat-refreshed
        // one: the comment heartbeat kept the watcher fresh.
        clk.advance(Duration::from_secs(2));
        w.refresh(clk.now());
        let (_, _, fresh, _) = w.snapshot(clk.now());
        assert!(fresh, "a comment heartbeat must keep the watcher fresh");
        w.close();
    }

    // Mirrors TestOpencodeWatcherCloseDuringRESTSeed (fix #10).
    #[test]
    fn close_during_rest_seed() {
        let f = FakeOpencode::new();
        f.set(|s| s.messages_body(OC_REST_MESSAGES));
        let entered = Arc::new(AtomicBool::new(false));
        let e = Arc::clone(&entered);
        f.before_messages(move |_call, conn| {
            e.store(true, Ordering::Relaxed);
            // Block the seed's /message read until the CLIENT disconnects
            // (Go blocks on the request ctx).
            while !conn.disconnected() {
                std::thread::sleep(Duration::from_millis(5));
            }
        });
        f.hold_open_sse();
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        poll_until("watcher reached the REST seed", || {
            entered.load(Ordering::Relaxed)
        });
        let w2 = Arc::clone(&w);
        let closer = std::thread::spawn(move || w2.close());
        assert!(closer.join().is_ok(), "close() must not block");
        assert!(
            w.wait_done(Duration::from_secs(2)),
            "thread leaked (run did not exit after close during a REST seed)"
        );
    }

    // Mirrors TestOpencodeWatcherInboxOverflow.
    #[test]
    fn inbox_overflow_forces_reconnect() {
        let f = FakeOpencode::new();
        f.set(|s| s.status_body(&busy_status_body()));
        f.on_event(move |conn, c| {
            c.write_sse(SSE_SERVER_CONNECTED);
            if conn == 1 {
                let frame = session_status_busy(SID);
                for _ in 0..(MAX_INBOX_ITEMS + 200) {
                    c.write_sse(&frame);
                }
            }
            c.hold_until_disconnect();
        });
        let clk = TestClock::new();
        let w = new_watcher(&f, DIR, SID, &clk);
        poll_until("overflow forces a reconnect+reseed", || {
            f.event_conns.load(Ordering::SeqCst) >= 2
        });
        w.refresh(clk.now()); // the overflowGap marker applies without panic
        w.close();
    }
}
