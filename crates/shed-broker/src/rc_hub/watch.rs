//! Watcher contracts: the freshness rule, the `mergedActivity` precedence
//! merge, the shared correlation helpers, and the tmux env seams — the pure
//! parts of `internal/ext/rc/watch.go` (plan 010 H4).
//!
//! The JSONL watchers are the structured-signal source that OVERRIDES the pane
//! stability engine for codex and claude sessions: instead of inferring
//! activity from whether the tmux pane keeps redrawing, they tail the agent's
//! own append-only log and read the turn/tool structure directly. opencode has
//! no log to tail — its watcher subscribes to the agent's embedded HTTP+SSE
//! server; cursor is push-fed by its own hook scripts POSTing into the hub's
//! ingest route. The hub merges a session's watcher with pane stability per
//! session: a fresh, correlated watcher wins; a broken/absent one falls back
//! to stability so activity never goes dark.
//!
//! From H5 it also carries the fold contracts ([`ActivityFold`] /
//! [`MessageProducer`] — Go's `activityFold`/`messageProducer` interfaces,
//! `watch.go:73`/`107`) and `listJSONLUnder`, consumed by the per-kind folds in
//! [`super::watch_claude`] / [`super::watch_codex`] / [`super::watch_cursor`] /
//! [`super::watch_opencode`]. Still Go-only until their commits: `fileWatcher`
//! + `fsNudger` (H7 — transports) and the opencode SSE transport (H8).

use std::path::Path;
use std::time::Duration;

use chrono::{DateTime, Utc};
use shed_core::rc::{RcActivity, RcKind};
use shed_core::rc_agents::{has_control_chars, parse_env, ENV_AGENT_SESSION, ENV_OPENCODE_PORT};
use shed_rc_engine::tmux::Tmux;

use super::messages::FeedMessage;

/// Folds a kind's parsed JSONL line stream into a live activity verdict
/// (`activityFold`, `watch.go:73`). Implementations hold cumulative state
/// across `apply_line` calls (turn boundaries, pending tool calls, the last
/// message) and are NOT safe for concurrent use — the owning watcher
/// serializes access.
pub trait ActivityFold {
    /// Folds one raw JSONL line, returning true when it advanced meaningful
    /// state (an activity-relevant event). Irrelevant/unparseable lines return
    /// false and leave state untouched (tolerant parsing).
    fn apply_line(&mut self, line: &[u8]) -> bool;
    /// Clears all state (the tailer reported a truncation/rotation).
    fn reset(&mut self);
    /// Tells the fold a record was LOST mid-stream (the tailer skipped an
    /// oversized line). Any state that depends on having seen every record —
    /// pending tool-call ids awaiting their output — must be dropped, leaving
    /// the verdict to coarser signals (turn boundaries) until the next turn
    /// re-establishes it.
    fn note_gap(&mut self);
    /// The current verdict: [`RcActivity::Unknown`] until a confirming event.
    fn activity(&self) -> RcActivity;
    /// A sanitized preview of the most recent agent message (`""` if none).
    fn last_message(&self) -> String;
    /// Whether the verdict is a terminal waiting state (needs_input/idle) —
    /// authoritative even when the file has gone quiet.
    fn settled(&self) -> bool;
    /// Go's runtime `messageProducer` type-assert on a fold
    /// (`(*fileWatcher).refresh`, `watch.go:195`), statically: a fold that
    /// also produces a feed overrides this to forward to
    /// [`MessageProducer::drain_messages`]; an activity-only fold (claude)
    /// inherits the empty default and contributes no feed rows.
    fn drain_fold_messages(&mut self) -> Vec<FeedMessage> {
        Vec::new()
    }
}

/// The narrow surface the reconcile loop and the input handler need from a
/// per-session watcher (`sessionWatcher`, `watch.go:120`): refresh it, read
/// its current verdict, drain any feed messages it produced, and check
/// whether it has ever folded an event. Implemented by [`FileWatcher`]
/// (codex/claude), the cursor watcher, and (H8) the opencode watcher, so
/// reconcile is transport-agnostic between a tailed JSONL file, a hook-push
/// inbox, and a live SSE feed.
///
/// `&self` receivers with interior locking mirror Go's pointer receivers over
/// an internal mutex; the `as_*` accessors mirror Go's runtime type-asserts
/// on the narrower capability interfaces (`cursorIngester`,
/// `confirmedAgentIDDrainer`; the approval pair arrives with the opencode
/// watcher in H8).
pub trait SessionWatcher: Send + Sync {
    /// Polls for new state and updates the watcher's current verdict. `now`
    /// stamps the last-event time used by the freshness decision.
    fn refresh(&self, now: DateTime<Utc>);
    /// The watcher's activity + message and its authority at `now`:
    /// `(activity, message, fresh, expired_working)`.
    fn snapshot(&self, now: DateTime<Utc>) -> (RcActivity, String, bool, bool);
    /// Returns and clears the feed messages produced since the last drain.
    fn drain_pending(&self) -> Vec<FeedMessage>;
    /// Whether the watcher has folded at least one activity-relevant event
    /// since it was created (used to confirm an ambiguous correlation).
    fn had_event(&self) -> bool;
    /// Releases the watcher's resources and marks it terminally closed.
    fn close(&self);
    /// Go's `cursorIngester` type-assert (`hub_ingest.go:140`).
    fn as_cursor_ingester(&self) -> Option<&dyn CursorIngester> {
        None
    }
    /// Go's `confirmedAgentIDDrainer` type-assert
    /// (`watch_opencode_transport.go:126`).
    fn as_confirmed_agent_id_drainer(&self) -> Option<&dyn ConfirmedAgentIdDrainer> {
        None
    }
    /// Go's `approvalPublisher` type-assert (`watch_opencode_transport.go:135`).
    fn as_approval_publisher(&self) -> Option<&dyn ApprovalPublisher> {
        None
    }
    /// Go's `approvalBlocker` type-assert (`watch_opencode_transport.go:143`).
    /// The claim seam ([`ClaimHolder`]) — opencode only; every other lane
    /// owns its conversation by construction.
    fn as_claim_holder(&self) -> Option<&dyn ClaimHolder> {
        None
    }

    fn as_approval_blocker(&self) -> Option<&dyn ApprovalBlocker> {
        None
    }
    /// Go's `turnStarter` type-assert (`hub_verbs.go:97`).
    fn as_turn_starter(&self) -> Option<&dyn super::verbs::TurnStarter> {
        None
    }
    /// Go's `turnInterrupter` type-assert (`hub_verbs.go:102`).
    fn as_turn_interrupter(&self) -> Option<&dyn super::verbs::TurnInterrupter> {
        None
    }
    /// Go's `approvalResolver` type-assert (`hub_verbs.go:113`).
    fn as_approval_resolver(&self) -> Option<&dyn super::verbs::ApprovalResolver> {
        None
    }
}

/// A watcher whose lane knows which approvals are still open, so reconcile can
/// publish them into the session's pending_approvals snapshot each tick
/// (`approvalPublisher`, `watch_opencode_transport.go:135`). PENDING ONLY —
/// resolution state stays in the watcher (approvalState), because the wire
/// contract defines pending_approvals as "what is still open", not an approval
/// log. Only the opencode watcher implements it today; a watcher that does not
/// leaves the snapshot untouched.
pub trait ApprovalPublisher {
    fn pending_approvals(&self) -> Vec<super::messages::FeedApproval>;
}

/// The input gate's counterpart to [`ApprovalPublisher`]: "is this session
/// currently blocked on an approval it would type an answer into?"
/// (`approvalBlocker`, `watch_opencode_transport.go:143`). Separate because it
/// is a STRICTLY WIDER question than the snapshot — it counts open questions
/// too, which are never addressable and so never appear in pending_approvals,
/// yet own the keyboard exactly the same.
pub trait ApprovalBlocker {
    fn has_open_approvals(&self) -> bool;
}

/// The narrow interface the hub's ingest handler pushes through
/// (`cursorIngester`, `watch_cursor.go:102`), so the handler holds a
/// [`SessionWatcher`] (as reconcile does) and asserts exactly this one
/// capability.
pub trait CursorIngester {
    /// Enqueues one hook event for the next refresh to fold, reporting
    /// whether it was accepted (false = the watcher is closed, or the inbox
    /// is full and the event was dropped).
    fn push_hook_event(&self, ev: super::watch_cursor::CursorHookEvent) -> bool;
}

/// A stream-discovered agent session id awaiting reconcile's
/// `SHED_RC_AGENT_SESSION` back-write (`confirmedAgentIDDrainer`,
/// `watch_opencode_transport.go:126`) — so a hub restart re-correlates
/// exactly. Implemented by the cursor watcher (hook-carried pins) and, at H8,
/// the opencode watcher; the file watchers correlate off-line.
pub trait ConfirmedAgentIdDrainer {
    /// Returns and clears a newly confirmed id ("" when none/already drained).
    fn drain_confirmed_agent_id(&self) -> String;
}

/// **One conversation, one owner.**
///
/// opencode servers are per-RC-session but read a SHARED per-project store, so
/// every watcher in a repository can see — and adopt — every other RC session's
/// conversation. Age alone cannot settle it: a session that starts FIRST and
/// stays idle will happily adopt the conversation a session started later is
/// actively using, because that conversation is newer than the adopter.
///
/// So the hub, which is the only party that can see all of them, tells each
/// watcher which ids are already spoken for. Pushed every tick rather than at
/// construction: the neighbour's pin usually does not exist yet when this
/// watcher is built.
pub trait ClaimHolder {
    /// The id this watcher has pinned, or "" while it is still searching.
    fn pinned_agent_id(&self) -> String;
    /// Ids pinned by OTHER sessions, which this watcher must never adopt.
    fn set_claimed(&self, ids: Vec<String>);
}

/// The hub's logger seam (Go's `func(string, ...any)`): pre-formatted lines,
/// no-op by default.
pub type LogFn = std::sync::Arc<dyn Fn(&str) + Send + Sync>;

/// A no-op [`LogFn`] (Go defaults a nil logf the same way).
pub fn noop_logf() -> LogFn {
    std::sync::Arc::new(|_| {})
}

/// A fold that ALSO produces a normalized message feed (`messageProducer`,
/// `watch.go:107`) — codex, opencode and cursor; claude feeds activity only in
/// this phase. Every watcher drains it on each refresh. It is a separate trait
/// from [`ActivityFold`] because the cursor fold produces a feed without being
/// an `ActivityFold` at all (its unit is a hook EVENT, not a JSONL line).
pub trait MessageProducer {
    /// Returns and clears the feed messages produced since the last drain.
    fn drain_messages(&mut self) -> Vec<FeedMessage>;
}

/// Walks `root` and returns every `*.jsonl` path, tolerating per-directory
/// permission errors (a skipped subdir does not abort the walk) —
/// `listJSONLUnder`, `watch.go:420`. `matches` filters basenames.
pub fn list_jsonl_under(root: &str, matches: impl Fn(&str) -> bool) -> Vec<String> {
    let mut out = Vec::new();
    // Go's filepath.WalkDir LSTATs the root: a symlinked root is not a
    // directory and yields nothing (fs::read_dir would happily follow it).
    if !std::fs::symlink_metadata(root).is_ok_and(|m| m.is_dir()) {
        return out;
    }
    walk_jsonl(Path::new(root), &matches, &mut out);
    out
}

fn walk_jsonl(dir: &Path, matches: &impl Fn(&str) -> bool, out: &mut Vec<String>) {
    let Ok(entries) = std::fs::read_dir(dir) else {
        return; // permission/transient on this dir → skip it, keep walking
    };
    // Sorted per directory, like Go's WalkDir: the result order feeds
    // correlate_codex's exact-id scan (first match wins), so iteration order
    // is contract — fs::read_dir alone is platform-arbitrary (H5 review).
    let mut entries: Vec<_> = entries.flatten().collect();
    entries.sort_by_key(std::fs::DirEntry::file_name);
    for entry in entries {
        let path = entry.path();
        let Ok(ft) = entry.file_type() else { continue };
        if ft.is_dir() {
            walk_jsonl(&path, matches, out);
            continue;
        }
        let Some(base) = path.file_name().and_then(|n| n.to_str()) else {
            continue;
        };
        // ends_with mirrors Go's filepath.Ext check, dotfiles included
        // (`filepath.Ext(".jsonl") == ".jsonl"`; Path::extension() sees none).
        if base.ends_with(".jsonl") && matches(base) {
            if let Some(p) = path.to_str() {
                out.push(p.to_string());
            }
        }
    }
}

/// Renders a raw JSON value as compact (whitespace-stripped) text — used for a
/// tool_use's input detail (`compactJSON`, `watch_opencode.go:932`; consumed by
/// the cursor and opencode folds). Mirrors Go's
/// `json.Compact`: the ORIGINAL bytes minus inter-token whitespace — no
/// reordering, no number reformatting — falling back to the trimmed raw text
/// when the value is not valid JSON.
pub(crate) fn compact_json(raw: &str) -> String {
    if raw.is_empty() {
        return String::new();
    }
    if serde_json::from_str::<serde::de::IgnoredAny>(raw).is_err() {
        return raw.trim().to_string();
    }
    // Strip whitespace outside string literals, byte-preserving inside them.
    let mut out = String::with_capacity(raw.len());
    let mut in_str = false;
    let mut escaped = false;
    for c in raw.chars() {
        if in_str {
            out.push(c);
            if escaped {
                escaped = false;
            } else if c == '\\' {
                escaped = true;
            } else if c == '"' {
                in_str = false;
            }
            continue;
        }
        match c {
            '"' => {
                in_str = true;
                out.push(c);
            }
            ' ' | '\t' | '\n' | '\r' => {}
            _ => out.push(c),
        }
    }
    out
}

/// Captures a raw field VERBATIM, `null` included (`Option<Box<RawValue>>`'s
/// stock decode maps `null` to `None`, but Go's `json.RawMessage` holds the
/// four bytes `null` and `compactJSON` renders them — a tool_input of `null`
/// must produce the detail `"null"`, not `""`; H5 review finding).
pub(crate) fn raw_opt<'de, D>(d: D) -> Result<Option<Box<serde_json::value::RawValue>>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    serde::Deserialize::deserialize(d).map(Some)
}

/// Captures any JSON value — `null` included — as `Some` (a stock
/// `Option<Value>` maps an explicit `null` to `None`, re-conflating it with an
/// absent field; Go's RawMessage keeps the two distinct).
pub(crate) fn value_opt<'de, D>(d: D) -> Result<Option<serde_json::Value>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    serde::Deserialize::deserialize(d).map(Some)
}

/// Decodes a nested OBJECT field with Go `encoding/json` semantics (H5 review
/// RES-3): an object decodes, `null` (or absent, via `#[serde(default)]`) is
/// `None`, and ANY other JSON shape errors — serde derives would otherwise
/// accept the positional seq/tuple form (`["user",…]`) that Go rejects.
/// Routed through a raw capture so `RawValue` fields inside `T` keep their
/// original bytes.
pub(crate) fn object_opt<'de, D, T>(d: D) -> Result<Option<T>, D::Error>
where
    D: serde::Deserializer<'de>,
    T: serde::de::DeserializeOwned,
{
    let raw: Box<serde_json::value::RawValue> = serde::Deserialize::deserialize(d)?;
    let s = raw.get().trim();
    if s.starts_with('{') {
        return serde_json::from_str::<T>(s)
            .map(Some)
            .map_err(serde::de::Error::custom);
    }
    if s == "null" {
        return Ok(None);
    }
    Err(serde::de::Error::custom("expected a JSON object"))
}

/// Decodes an array-of-objects field with Go semantics (H5 review RES-3):
/// `null` is the nil slice (empty), every element must be an OBJECT (Go's
/// whole-array unmarshal errors on a positional-form element where serde
/// derives would accept it), and a non-array errors. Raw-routed so `RawValue`
/// fields inside `T` keep their bytes.
pub(crate) fn vec_objects<'de, D, T>(d: D) -> Result<Vec<T>, D::Error>
where
    D: serde::Deserializer<'de>,
    T: serde::de::DeserializeOwned + Default,
{
    let raws: Option<Vec<Box<serde_json::value::RawValue>>> = serde::Deserialize::deserialize(d)?;
    let Some(raws) = raws else {
        return Ok(Vec::new());
    };
    raws.into_iter()
        .map(|r| {
            let s = r.get().trim();
            if s.starts_with('{') {
                return serde_json::from_str::<T>(s).map_err(serde::de::Error::custom);
            }
            if s == "null" {
                // Go's null-is-a-no-op applies at EVERY level, array elements
                // included: a null element decodes to the zero value (H6
                // review, HIGH).
                return Ok(T::default());
            }
            Err(serde::de::Error::custom("expected a JSON object element"))
        })
        .collect()
}

/// A `Vec<String>` field with FULL Go null semantics (H6 review, HIGH): the
/// field itself may be `null` (nil slice → empty) and so may any ELEMENT
/// (Go's `[]string` decodes a null element as `""`); a wrong-typed element
/// still errors, like Go.
pub(crate) fn null_string_vec<'de, D>(d: D) -> Result<Vec<String>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    let v: Option<Vec<Option<String>>> = serde::Deserialize::deserialize(d)?;
    Ok(v.unwrap_or_default()
        .into_iter()
        .map(Option::unwrap_or_default)
        .collect())
}

/// [`object_opt`] for a NON-pointer nested struct field (Go's value-typed
/// nested structs, e.g. a part's `time`): an object decodes, `null` no-ops to
/// the zero value, any other shape errors.
pub(crate) fn object_default<'de, D, T>(d: D) -> Result<T, D::Error>
where
    D: serde::Deserializer<'de>,
    T: serde::de::DeserializeOwned + Default,
{
    Ok(object_opt(d)?.unwrap_or_default())
}

/// The first non-whitespace byte of a JSON document (Go's decoder skips
/// exactly space/tab/newline/CR before the value). The folds gate their
/// top-level struct decodes on `Some(b'{')` — Go's `Unmarshal` into a struct
/// rejects any other shape (and a top-level `null` no-ops into the zero
/// value, which every top-level call site's zero value routes to the same
/// "not folded" outcome).
pub(crate) fn json_first_byte(line: &[u8]) -> Option<u8> {
    line.iter()
        .copied()
        .find(|b| !matches!(b, b' ' | b'\t' | b'\n' | b'\r'))
}

/// `firstNonEmpty` (`ops.go:480`) for the two-candidate case every fold call
/// site has.
pub(crate) fn first_non_empty<'a>(a: &'a str, b: &'a str) -> &'a str {
    if !a.is_empty() {
        a
    } else {
        b
    }
}

// ---------------------------------------------------------------------------
// fileWatcher (watch.go:137-271) — the tailer+fold transport for codex/claude
// ---------------------------------------------------------------------------

use std::sync::Mutex;

use super::tail::LineTailer;

/// Pairs a tailer with a fold and tracks freshness for the reconcile merge
/// (`fileWatcher`, `watch.go:137`). `&self` methods over an internal mutex
/// mirror Go's `mu`-guarded pointer receivers.
pub struct FileWatcher {
    inner: Mutex<FileWatcherInner>,
}

struct FileWatcherInner {
    tailer: LineTailer,
    fold: Box<dyn ActivityFold + Send>,
    last_event_at: Option<DateTime<Utc>>,
    cur_activity: RcActivity,
    cur_message: String,
    cur_settled: bool,
    /// Feed messages produced since the last drain_pending.
    pending: Vec<FeedMessage>,
    /// Terminal: refresh no-ops after close (see [`SessionWatcher::close`]).
    closed: bool,
}

impl FileWatcher {
    /// `newFileWatcher`, `watch.go:154`.
    pub fn new(path: &str, catch_up: bool, fold: Box<dyn ActivityFold + Send>) -> FileWatcher {
        FileWatcher {
            inner: Mutex::new(FileWatcherInner {
                tailer: LineTailer::new(path, catch_up),
                fold,
                last_event_at: None,
                cur_activity: RcActivity::Unknown,
                cur_message: String::new(),
                cur_settled: false,
                pending: Vec::new(),
                closed: false,
            }),
        }
    }

    /// Whether the tailer currently holds an open handle (the closed-refresh
    /// no-op pin reads it; Go tests reach `w.tailer.f` directly).
    #[cfg(test)]
    pub(crate) fn tailer_is_open(&self) -> bool {
        self.inner
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .tailer
            .is_open()
    }
}

impl SessionWatcher for FileWatcher {
    /// Polls the file and folds any new lines (`(*fileWatcher).refresh`,
    /// `watch.go:167`). A reset from the tailer clears the fold; a poll error
    /// (permission/transient) is swallowed so the prior verdict is retained.
    /// `now` stamps the last-event time used by the freshness decision. A
    /// CLOSED watcher no-ops: the tailer released its file handle on close,
    /// and a poll would silently reopen the path from offset 0 — a full
    /// re-read (and a leaked handle) that refolds a dead incarnation's
    /// history into a watcher that is already discarded.
    fn refresh(&self, now: DateTime<Utc>) {
        let w = &mut *self
            .inner
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if w.closed {
            return;
        }
        let poll = w.tailer.poll();
        if poll.did_reset {
            w.fold.reset();
        }
        if poll.gapped {
            // A record was lost (oversized skip): drop record-exact state
            // (pending tool calls) so a swallowed *_output line can't pin the
            // verdict at working forever.
            w.fold.note_gap();
        }
        if poll.err.is_some() {
            return;
        }
        for ln in &poll.lines {
            if w.fold.apply_line(ln) {
                w.last_event_at = Some(now);
            }
        }
        w.cur_activity = w.fold.activity();
        w.cur_message = w.fold.last_message();
        w.cur_settled = w.fold.settled();
        // Drain any feed messages the fold produced this poll into the
        // watcher's pending queue; reconcile empties it into the session ring.
        let msgs = w.fold.drain_fold_messages();
        w.pending.extend(msgs);
    }

    /// The watcher's activity + message and its authority at `now` (the
    /// shared [`watcher_freshness`] rule — a tailed file is quiet or it is
    /// not; there is no transport-health dimension here) —
    /// `(*fileWatcher).snapshot`, `watch.go:245`.
    fn snapshot(&self, now: DateTime<Utc>) -> (RcActivity, String, bool, bool) {
        let w = self
            .inner
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let (fresh, expired_working) =
            watcher_freshness(w.cur_activity, w.cur_settled, w.last_event_at, now);
        (
            w.cur_activity,
            w.cur_message.clone(),
            fresh,
            expired_working,
        )
    }

    /// Returns and clears the feed messages produced since the last drain, in
    /// stream order (`drainPending`, `watch.go:202`).
    fn drain_pending(&self) -> Vec<FeedMessage> {
        let mut w = self
            .inner
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        std::mem::take(&mut w.pending)
    }

    /// Whether the fold has consumed at least one activity-relevant event
    /// since attach (`hadEvent`, `watch.go:257`). Used to confirm an
    /// AMBIGUOUS correlation before its session id is back-written: an
    /// in-file event after attach is the "first in-file event confirms"
    /// signal (the watcher is follow-only on the ambiguous path, so any
    /// folded event necessarily happened after this session was created).
    fn had_event(&self) -> bool {
        self.inner
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .last_event_at
            .is_some()
    }

    /// Releases the tailer's file handle and marks the watcher terminally
    /// closed (`close`, `watch.go:266`) — any later refresh (e.g. an input
    /// handler holding a stale pointer) is a no-op rather than a from-zero
    /// reopen. Idempotent.
    fn close(&self) {
        let mut w = self
            .inner
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        w.closed = true;
        w.tailer.close();
    }
}

// ---------------------------------------------------------------------------
// fsnotify nudge layer (watch.go:438-564) over the `notify` crate
// ---------------------------------------------------------------------------

/// Watches the codex + claude root trees and pings a channel whenever a file
/// changes, so the hub can run a reconcile sub-tick (`fsNudger`,
/// `watch.go:447`) — activity surfaces promptly instead of waiting up to the
/// active interval. It is a best-effort LATENCY optimization: the reconcile
/// tick already refreshes every watcher, so a missed notification only delays
/// a transition to the next tick. Watching is non-recursive, so directories
/// are added as they appear (codex's dated YYYY/MM/DD subdirs, or the whole
/// ~/.codex tree on a fresh machine).
///
/// Shape delta vs Go (documented, not parity debt): Go runs a goroutine
/// selecting over fsnotify's channels until ctx cancellation; `notify`
/// delivers events through a channel too, and the loop here is a dedicated
/// thread draining it with a bounded `recv_timeout` (std has no `select`).
/// The stop flag is read as the FIRST statement of every iteration, before
/// the receive — so it is honored within ~100ms whether the stream is quiet
/// OR saturated. (Checking it only on the timeout arm would let a busy tree
/// starve the check forever, and since the thread owns the watcher — hence
/// the sender — the channel would never disconnect to break the tie either,
/// so [`FsNudger::stop`] would block in `join` for as long as writes kept
/// arriving.) Stopping is explicit ([`FsNudger::stop`], also called on drop).
pub struct FsNudger {
    nudge_rx: std::sync::mpsc::Receiver<()>,
    stop: std::sync::Arc<std::sync::atomic::AtomicBool>,
    handle: Option<std::thread::JoinHandle<()>>,
}

/// The watcher + added-set state the nudger thread owns (Go keeps these as
/// `fsNudger` fields guarded by `mu`; here the thread owns them outright and
/// the tests drive the struct directly).
pub(crate) struct NudgerState {
    watcher: notify::RecommendedWatcher,
    added: std::collections::HashSet<std::path::PathBuf>,
}

impl NudgerState {
    /// Adds a watch on dir and every existing subdirectory (`addTree`,
    /// `watch.go:477` — the backend watch is non-recursive). Missing dirs and
    /// permission errors are ignored — a dir that appears later is picked up
    /// by the Create handler in the run loop.
    fn add_tree(&mut self, dir: &Path) {
        self.add_dir(dir);
        let Ok(entries) = std::fs::read_dir(dir) else {
            return;
        };
        for entry in entries.flatten() {
            if entry.file_type().is_ok_and(|t| t.is_dir()) {
                self.add_tree(&entry.path());
            }
        }
    }

    /// `addDir`, `watch.go:489`. Records only on a SUCCESSFUL add — a failed
    /// add must stay forgettable so a later retry (e.g. after the dir becomes
    /// readable) can go through.
    fn add_dir(&mut self, path: &Path) {
        if self.added.contains(path) {
            return;
        }
        if notify::Watcher::watch(&mut self.watcher, path, notify::RecursiveMode::NonRecursive)
            .is_err()
        {
            return;
        }
        self.added.insert(path.to_path_buf());
    }

    /// Drops path (and everything under it) from the added set when the dir
    /// is removed or renamed away (`forgetDir`, `watch.go:507`) — the backend
    /// silently drops the kernel watch for a deleted dir, so without this a
    /// recreated dir at the same path would be skipped by add_dir's dedupe
    /// and its writes would nudge nothing until the next full tick.
    fn forget_dir(&mut self, path: &Path) {
        // `Path::starts_with` is COMPONENT-wise and matches `path` itself, so
        // this one retain covers both halves of Go's delete-then-prefix-sweep
        // (`/a` and `/a/b` go; `/ab` stays).
        self.added.retain(|p| !p.starts_with(path));
    }

    #[cfg(test)]
    pub(crate) fn contains(&self, path: &Path) -> bool {
        self.added.contains(path)
    }
}

impl FsNudger {
    /// Builds a nudger over the given roots and starts its thread
    /// (`newFSNudger` + `run`, `watch.go:460`/`521`). It never fails the
    /// caller beyond construction: if the backend is unavailable, the error
    /// surfaces here and the reconcile tick is the sole driver.
    pub fn new(roots: &[String], logf: LogFn) -> Result<FsNudger, notify::Error> {
        let (event_tx, event_rx) = std::sync::mpsc::channel::<notify::Result<notify::Event>>();
        let watcher = notify::recommended_watcher(event_tx)?;
        // The ROOT watches are added synchronously, before the loop thread
        // spawns: backend stream startup (FSEvents in particular) is slow
        // enough that deferring them to the thread races anything created
        // right after construction — exactly the window the nudger exists
        // for. (Go adds them inside run(); its fsnotify starts fast enough
        // that the difference is unobservable there.)
        let mut state = NudgerState {
            watcher,
            added: std::collections::HashSet::new(),
        };
        for root in roots {
            state.add_tree(Path::new(root));
        }
        // Coalesced cap-1 nudge: a pending nudge absorbs bursts (`signal`,
        // `watch.go:559`).
        let (nudge_tx, nudge_rx) = std::sync::mpsc::sync_channel::<()>(1);
        let stop = std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false));
        let stop_flag = std::sync::Arc::clone(&stop);
        let handle = std::thread::Builder::new()
            .name("rc-hub-fsnudge".into())
            .spawn(move || {
                loop {
                    // FIRST, unconditionally: a saturated event stream must not
                    // starve the stop check (see the shape-delta note on
                    // FsNudger).
                    if stop_flag.load(std::sync::atomic::Ordering::Relaxed) {
                        return;
                    }
                    match event_rx.recv_timeout(std::time::Duration::from_millis(100)) {
                        Ok(Ok(ev)) => {
                            if matches!(ev.kind, notify::EventKind::Create(_)) {
                                // A new dated subdir (or the sessions/projects
                                // dir itself) — start watching it so its
                                // files' writes are seen.
                                for path in &ev.paths {
                                    if std::fs::metadata(path).is_ok_and(|m| m.is_dir()) {
                                        state.add_tree(path);
                                    }
                                }
                            }
                            if matches!(
                                ev.kind,
                                notify::EventKind::Remove(_)
                                    | notify::EventKind::Modify(notify::event::ModifyKind::Name(_))
                            ) {
                                // Rename/remove: notify reports a rename in
                                // EITHER direction as Modify(Name(_)) (where
                                // Go's fsnotify reports a rename INTO the tree
                                // as CREATE), so stat decides: a path that
                                // still exists as a directory was renamed IN —
                                // start watching it, like Go's Create arm (H7
                                // review); a path that is gone is forgotten so
                                // a recreation at the same path can be
                                // re-added.
                                for path in &ev.paths {
                                    if std::fs::metadata(path).is_ok_and(|m| m.is_dir()) {
                                        state.add_tree(path);
                                    } else {
                                        state.forget_dir(path);
                                    }
                                }
                            }
                            let _ = nudge_tx.try_send(());
                        }
                        Ok(Err(err)) => logf(&format!("rc hub: fsnotify error: {err}")),
                        // Nothing arrived inside the bound: loop back around to
                        // the stop check above.
                        Err(std::sync::mpsc::RecvTimeoutError::Timeout) => {}
                        Err(std::sync::mpsc::RecvTimeoutError::Disconnected) => return,
                    }
                }
            })
            .expect("spawn fsnudge thread");
        Ok(FsNudger {
            nudge_rx,
            stop,
            handle: Some(handle),
        })
    }

    /// The coalesced nudge channel reconcile selects on.
    pub fn nudge(&self) -> &std::sync::mpsc::Receiver<()> {
        &self.nudge_rx
    }

    /// Stops the nudger thread (joins; the watcher is dropped with it).
    pub fn stop(&mut self) {
        self.stop.store(true, std::sync::atomic::Ordering::Relaxed);
        if let Some(handle) = self.handle.take() {
            let _ = handle.join();
        }
    }
}

impl Drop for FsNudger {
    fn drop(&mut self) {
        self.stop();
    }
}

/// Test-only helpers shared by the per-kind fold test mods (the Go suite gets
/// these from being one package; here they are the one local home so the fold
/// test mods stay pure scenario code).
#[cfg(test)]
pub(crate) mod test_support {
    use chrono::{DateTime, TimeZone, Utc};

    /// A `GetEnv` answering HOME from a tempdir and `""` for everything else —
    /// the Go suite's `t.Setenv("HOME", dir)` fixture.
    pub(crate) fn home_getenv(home: &std::path::Path) -> impl Fn(&str) -> String {
        let home = home.to_str().expect("utf-8 tempdir").to_string();
        move |k: &str| {
            if k == "HOME" {
                home.clone()
            } else {
                String::new()
            }
        }
    }

    /// The non-blank lines of a shared JSONL fixture (`crates/fixtures/jsonl`).
    pub(crate) fn fixture_lines(name: &str) -> Vec<Vec<u8>> {
        let path = format!("{}/../fixtures/jsonl/{name}", env!("CARGO_MANIFEST_DIR"));
        let data = std::fs::read(&path).expect("fixture readable");
        data.split(|&b| b == b'\n')
            .filter(|l| !l.iter().all(u8::is_ascii_whitespace))
            .map(<[u8]>::to_vec)
            .collect()
    }

    /// The correlation fixtures' reference created-at (`watch_test.go`'s
    /// `base`). Also the pre-watcher queue suite's clock origin — the ingest
    /// tests share it rather than restating the timestamp.
    pub(crate) fn base_time() -> DateTime<Utc> {
        Utc.with_ymd_and_hms(2026, 7, 11, 17, 0, 0).unwrap()
    }

    /// The WATCHER suites' clock origin (distinct from [`base_time`], which is
    /// pinned to the correlation fixtures' created-at): any fixed instant
    /// works, since every watcher assertion is relative to it.
    pub(crate) fn t0() -> DateTime<Utc> {
        DateTime::from_timestamp(1_700_000_000, 0).expect("valid epoch")
    }

    /// `base` advanced by a [`std::time::Duration`] — the freshness/grace
    /// assertions' "now + window" idiom.
    pub(crate) fn plus(base: DateTime<Utc>, d: std::time::Duration) -> DateTime<Utc> {
        base + chrono::Duration::from_std(d).expect("in range")
    }
}

/// Bounds how long a correlated watcher's non-settled, non-working activity is
/// trusted after its last folded event (`watcherFreshWindow`, `watch.go:53`).
/// A settled verdict (needs_input/idle) stays authoritative indefinitely — a
/// quiet file is exactly what a waiting agent produces — so in practice this
/// window governs only transitional verdicts.
pub const WATCHER_FRESH_WINDOW: Duration = Duration::from_secs(30);

/// The DELIBERATELY LONGER quiet tolerance for a working verdict
/// (`watcherWorkingGrace`, `watch.go:63`): a long tool call or model turn can
/// legitimately write nothing to the JSONL for tens of seconds, and flipping
/// to stability at 30s would flap a mid-turn session. The asymmetry with
/// [`WATCHER_FRESH_WINDOW`] is intentional: needs_input/idle keep the 30s rule
/// (they are settled anyway), working gets 120s — and even past 120s, working
/// only yields to stability when stability itself holds a SETTLED quiet
/// verdict (see [`merged_activity`]).
pub const WATCHER_WORKING_GRACE: Duration = Duration::from_secs(120);

/// The ±tolerance around a session's created-at within which a candidate JSONL
/// file's own creation time must fall to be a match (`correlateWindow`,
/// `watch.go:67`).
pub const CORRELATE_WINDOW: Duration = Duration::from_secs(60);

/// THE quiet-source freshness rule (`watcherFreshness`, `watch.go:227`),
/// shared verbatim by every watcher that has one (the file watcher, the
/// opencode watcher once its transport is healthy, the cursor watcher on its
/// pushes). Given a verdict, whether it is settled, and when the source last
/// produced an event, it reports the verdict's authority at `now`:
///
/// - `fresh`: authoritative outright — settled (needs_input/idle; trusted
///   indefinitely), recent (last event within [`WATCHER_FRESH_WINDOW`]), or
///   working within [`WATCHER_WORKING_GRACE`].
/// - `expired_working`: a working verdict whose source has been quiet past the
///   grace — not discarded, but demoted to conditional: the merge lets
///   stability take over only if stability holds a settled quiet verdict.
///
/// An unknown verdict is never fresh (Go's empty Activity folds into
/// [`RcActivity::Unknown`] here — the two behave identically in every arm). A
/// `None` `last_event_at` means "nothing folded yet" (Go's zero `time.Time`),
/// which is neither recent nor within the grace.
pub fn watcher_freshness(
    activity: RcActivity,
    settled: bool,
    last_event_at: Option<DateTime<Utc>>,
    now: DateTime<Utc>,
) -> (bool, bool) {
    if activity == RcActivity::Unknown {
        return (false, false);
    }
    // Go models "no event yet" as sinceEvent = -1; an Option carries the same
    // "neither recent nor in grace" through the is_some_and arms. A negative
    // elapsed (event stamped ahead of now) is likewise not recent, matching
    // Go's `sinceEvent >= 0` guards.
    let since_event = last_event_at.map(|t| now.signed_duration_since(t));
    let within = |window: Duration| {
        since_event
            .is_some_and(|d| d >= chrono::Duration::zero() && d.to_std().is_ok_and(|d| d < window))
    };
    let recent = within(WATCHER_FRESH_WINDOW);
    let working_grace = activity == RcActivity::Working && within(WATCHER_WORKING_GRACE);
    let fresh = settled || recent || working_grace;
    let expired_working = activity == RcActivity::Working && !fresh;
    (fresh, expired_working)
}

/// Resolves the reconcile precedence (`mergedActivity`, `watch.go:285`):
///
/// - a FRESH watcher verdict (and its last-message) wins outright;
/// - an EXPIRED-WORKING verdict (working, file quiet past the grace) yields to
///   stability only when stability holds a settled quiet verdict
///   (idle/needs_input — the pane genuinely stopped); if the pane still churns
///   (stability=working) or stability has no verdict, working is KEPT — a long
///   silent turn must not flap;
/// - otherwise the pane-stability activity drives and last-message is dropped
///   (stability has no message signal).
///
/// Returned activity is still subject to the lifecycle-trumps display rule by
/// the caller.
pub fn merged_activity(
    watcher_activity: RcActivity,
    watcher_message: &str,
    watcher_fresh: bool,
    watcher_expired_working: bool,
    stability: RcActivity,
) -> (RcActivity, String) {
    if watcher_fresh {
        return (watcher_activity, watcher_message.to_string());
    }
    if watcher_expired_working {
        if stability == RcActivity::Idle || stability == RcActivity::NeedsInput {
            return (stability, String::new());
        }
        return (watcher_activity, watcher_message.to_string());
    }
    (stability, String::new())
}

/// Whether a kind has a structured-signal watcher (`watchableKind`,
/// `watch.go:303`): codex/claude tail a JSONL file, opencode subscribes to its
/// embedded HTTP+SSE server, and cursor is fed by its own hook scripts pushing
/// into the hub's ingest route. Other kinds derive activity from pane
/// stability alone.
pub fn watchable_kind(k: &RcKind) -> bool {
    matches!(k, RcKind::Codex | RcKind::Opencode | RcKind::Cursor) || k.runs_claude()
}

/// The outcome of mapping a tmux session to its agent JSONL file
/// (`correlation`, `watch.go:308`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Correlation {
    /// The chosen file.
    pub path: String,
    /// The agent's own session id (back-written into the tmux env).
    pub session_id: String,
    /// More than one candidate in the window → newest chosen, treat history as
    /// untrusted.
    pub ambiguous: bool,
}

/// The correlation metadata read from an agent JSONL file's early lines
/// (codex rollout `session_meta` / claude transcript header) — `jsonlPeek`,
/// `watch.go:317`. Both per-kind peek parsers return it so the newest-pick +
/// ambiguity logic below is shared. Go's `createdAt`+`hasTime` pair is an
/// `Option` here.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct JsonlPeek {
    pub session_id: String,
    pub cwd: String,
    pub created_at: Option<DateTime<Utc>>,
}

/// A JSONL file paired with its peeked correlation metadata (`peekCandidate`,
/// `watch.go:325`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PeekCandidate {
    pub path: String,
    pub peek: JsonlPeek,
}

/// Whether candidate `a` is newer than `b` by peeked created-at (`peekNewer`,
/// `watch.go:336`). Window candidates always carry a created-at (no-timestamp
/// files are excluded from window matching by the correlate functions).
/// `name_tiebreak` breaks an exact created-at tie by filename; only codex
/// passes true (rollout names are timestamp-prefixed, so lexical order is
/// chronological) — claude transcript names are bare UUIDs, where a filename
/// comparison would be meaningless.
pub fn peek_newer(a: &PeekCandidate, b: &PeekCandidate, name_tiebreak: bool) -> bool {
    if a.peek.created_at != b.peek.created_at {
        return a.peek.created_at > b.peek.created_at;
    }
    if name_tiebreak {
        return base_name(&a.path) > base_name(&b.path);
    }
    false
}

fn base_name(path: &str) -> &str {
    Path::new(path)
        .file_name()
        .and_then(|n| n.to_str())
        .unwrap_or(path)
}

/// The correlation for the newest of `matches`, flagging ambiguity when more
/// than one candidate survived the caller's window filter (history untrusted)
/// — `pickCorrelation`, `watch.go:349`. `matches` must be non-empty, exactly
/// as in Go.
pub fn pick_correlation(matches: &[PeekCandidate], name_tiebreak: bool) -> Correlation {
    let mut best = 0;
    for i in 1..matches.len() {
        if peek_newer(&matches[i], &matches[best], name_tiebreak) {
            best = i;
        }
    }
    Correlation {
        path: matches[best].path.clone(),
        session_id: matches[best].peek.session_id.clone(),
        ambiguous: matches.len() > 1,
    }
}

/// Whether `a` and `b` are within `w` of each other (`withinWindow`,
/// `watch.go:364`).
///
/// One unreachable divergence, recorded for the differential: Go's `a.Sub(b)`
/// saturates at ±292 years and its `d = -d` negation of `MinInt64` stays
/// negative, so Go answers `true` for a zero-time vs modern-time pair where
/// this (correctly) answers `false`. Every Go caller guards with `hasTime`
/// first, so only a fixture stamped year 0001/9999 could ever observe it.
pub fn within_window(a: DateTime<Utc>, b: DateTime<Utc>, w: Duration) -> bool {
    let d = a.signed_duration_since(b).abs();
    d.to_std().is_ok_and(|d| d <= w)
}

/// Parses an RFC3339(nano) timestamp; `None` on empty/invalid
/// (`parseJSONLTime`, `watch.go:373`).
///
/// KNOWN acceptance deltas vs Go `time.Parse(time.RFC3339Nano)` (H4 review,
/// 42-shape probe): chrono additionally accepts lowercase `t`/`z`, a space
/// separator, leap-second `:60`, and a U+2212 minus in the offset; Go
/// additionally accepts a comma fraction, a 1-digit hour, and `+24:00`
/// offsets. No real producer emits any of these shapes (codex stamps via
/// chrono, claude via JS `toISOString()`), so the delta is left undocumented
/// in behavior rather than papered over with pre-filters; the H5 correlate
/// differential cells are the tripwire if a producer ever changes.
pub fn parse_jsonl_time(s: &str) -> Option<DateTime<Utc>> {
    if s.is_empty() {
        return None;
    }
    DateTime::parse_from_rfc3339(s)
        .ok()
        .map(|t| t.with_timezone(&Utc))
}

/// Reads the back-written `SHED_RC_AGENT_SESSION` for a tmux session (`""`
/// when absent) — `agentSessionEnv`, `watch.go:385`. It rides
/// `show_environment`'s `SHED_RC_` filter.
pub fn agent_session_env(tmux: &Tmux<'_>, tmux_name: &str) -> String {
    parse_env(&tmux.show_environment(tmux_name))
        .get(ENV_AGENT_SESSION)
        .cloned()
        .unwrap_or_default()
}

/// Reads the create-time `SHED_RC_OPENCODE_PORT` for a tmux session (stamped
/// by the create-side env args) and range-validates it (`opencodePortEnv`,
/// `watch.go:398`): a missing key, a value that doesn't parse as an integer,
/// or one outside 1..=65535 all report `None` — the session is unwatchable
/// over the opencode SSE transport (a pre-upgrade session created before this
/// port plumbing shipped simply never had the key stamped, which is exactly
/// this "missing" case). Go's `(int, bool)` pair is an `Option<u16>` here —
/// the range check makes the narrower type exact.
pub fn opencode_port_env(tmux: &Tmux<'_>, tmux_name: &str) -> Option<u16> {
    parse_env(&tmux.show_environment(tmux_name))
        .get(ENV_OPENCODE_PORT)
        .and_then(|raw| raw.parse::<i64>().ok())
        .filter(|port| (1..=65535).contains(port))
        .map(|port| port as u16)
}

/// Stamps `SHED_RC_AGENT_SESSION` into the tmux session env so a hub restart
/// re-correlates exactly (`backWriteAgentSession`, `watch.go:411`).
/// Best-effort: a set-environment failure is swallowed (the window heuristic
/// re-runs next time). Control-char-guarded like every other `SHED_RC_` value.
pub fn back_write_agent_session(tmux: &Tmux<'_>, tmux_name: &str, id: &str) {
    if id.is_empty() || has_control_chars(id) {
        return;
    }
    let _ = tmux.set_environment(tmux_name, ENV_AGENT_SESSION, id);
}

#[cfg(test)]
mod tests {
    use super::test_support::{plus, t0};
    use super::*;
    use chrono::TimeZone;
    use shed_rc_engine::tmux::{TmuxResult, TmuxRunner};
    use std::collections::HashMap;
    use std::sync::Mutex;

    // The freshness RULE, mirrored directly (the Go suite pins it through
    // fileWatcher + a codex fold in
    // TestFileWatcherFreshnessSettledVsWorkingGrace; the fold-free rule is
    // what H4 owns — the fileWatcher wrapper re-pins it in H7).
    #[test]
    fn freshness_settled_vs_working_grace() {
        let now = t0();

        // Settled (needs_input) stays authoritative even long after the last
        // event.
        let (fresh, _) = watcher_freshness(RcActivity::NeedsInput, true, Some(now), now);
        assert!(fresh, "settled is fresh at t0");
        let (fresh, expired) = watcher_freshness(
            RcActivity::NeedsInput,
            true,
            Some(now),
            plus(now, Duration::from_secs(600)),
        );
        assert!(
            fresh && !expired,
            "settled stays fresh while the file is quiet"
        );

        // Working keeps its authority through the LONG grace…
        let (fresh, expired) = watcher_freshness(
            RcActivity::Working,
            false,
            Some(now),
            plus(now, WATCHER_FRESH_WINDOW + Duration::from_secs(1)),
        );
        assert!(fresh && !expired, "working inside the grace stays fresh");
        // …and only past the grace demotes to expired_working (still not
        // dropped — the merge decides against stability's verdict).
        let (fresh, expired) = watcher_freshness(
            RcActivity::Working,
            false,
            Some(now),
            plus(now, WATCHER_WORKING_GRACE + Duration::from_secs(1)),
        );
        assert!(
            !fresh && expired,
            "working past the grace is expired_working"
        );

        // A transitional non-working verdict follows the 30s window only.
        let (fresh, expired) = watcher_freshness(
            RcActivity::Idle,
            false,
            Some(now),
            plus(now, Duration::from_secs(10)),
        );
        assert!(fresh && !expired, "recent transitional verdict is fresh");
        let (fresh, expired) = watcher_freshness(
            RcActivity::Idle,
            false,
            Some(now),
            plus(now, WATCHER_FRESH_WINDOW + Duration::from_secs(1)),
        );
        assert!(!fresh && !expired, "stale transitional verdict is neither");

        // An unknown verdict is never fresh; "nothing folded yet" is neither
        // recent nor in grace.
        assert_eq!(
            watcher_freshness(RcActivity::Unknown, true, Some(now), now),
            (false, false)
        );
        assert_eq!(
            watcher_freshness(RcActivity::Working, false, None, now),
            (false, true),
            "working with no event ever is expired_working"
        );
    }

    // Mirrors TestMergedActivityPrecedence.
    #[test]
    fn merged_activity_precedence() {
        // Fresh watcher wins (activity + message).
        assert_eq!(
            merged_activity(RcActivity::Working, "hello", true, false, RcActivity::Idle),
            (RcActivity::Working, "hello".to_string())
        );
        // Stale (non-working) watcher → stability drives and the message is
        // dropped.
        assert_eq!(
            merged_activity(RcActivity::Unknown, "hello", false, false, RcActivity::Idle),
            (RcActivity::Idle, String::new())
        );
        // Expired working + stability SETTLED quiet (idle/needs_input) →
        // stability wins.
        assert_eq!(
            merged_activity(
                RcActivity::Working,
                "hello",
                false,
                true,
                RcActivity::NeedsInput
            ),
            (RcActivity::NeedsInput, String::new())
        );
        assert_eq!(
            merged_activity(RcActivity::Working, "hello", false, true, RcActivity::Idle).0,
            RcActivity::Idle
        );
        // Expired working + stability still churning (working) → keep working
        // (no flap).
        assert_eq!(
            merged_activity(
                RcActivity::Working,
                "hello",
                false,
                true,
                RcActivity::Working
            ),
            (RcActivity::Working, "hello".to_string())
        );
        // Expired working + stability has no verdict → keep working too.
        assert_eq!(
            merged_activity(
                RcActivity::Working,
                "hello",
                false,
                true,
                RcActivity::Unknown
            )
            .0,
            RcActivity::Working
        );
    }

    // Mirrors TestWatchableKindOpencode (extended over the full kind axis —
    // the Go arm asserts opencode in and shell out).
    #[test]
    fn watchable_kinds() {
        assert!(watchable_kind(&RcKind::Codex));
        assert!(watchable_kind(&RcKind::ClaudeRc));
        assert!(watchable_kind(&RcKind::ClaudeBroker));
        assert!(watchable_kind(&RcKind::Opencode));
        assert!(watchable_kind(&RcKind::Cursor));
        assert!(!watchable_kind(&RcKind::Shell), "shell is stability only");
        assert!(!watchable_kind(&RcKind::Other("mystery".into())));
    }

    fn cand(path: &str, session_id: &str, created_at: Option<DateTime<Utc>>) -> PeekCandidate {
        PeekCandidate {
            path: path.to_string(),
            peek: JsonlPeek {
                session_id: session_id.to_string(),
                cwd: "/home/shed".to_string(),
                created_at,
            },
        }
    }

    // The shared newest-pick + ambiguity logic the per-kind correlate
    // functions (H5) sit on — the helper half of the Go correlation suite
    // (TestCorrelateCodexTwoSessionsOneWorkdir's pick semantics, fold-free).
    #[test]
    fn pick_correlation_newest_and_ambiguity() {
        let base = t0();
        let older = cand("/r/rollout-a.jsonl", "aaaa-a", Some(base));
        let newer = cand(
            "/r/rollout-b.jsonl",
            "bbbb-b",
            Some(plus(base, Duration::from_secs(20))),
        );

        // A single candidate is unambiguous.
        let corr = pick_correlation(std::slice::from_ref(&older), true);
        assert_eq!(
            corr,
            Correlation {
                path: "/r/rollout-a.jsonl".into(),
                session_id: "aaaa-a".into(),
                ambiguous: false,
            }
        );

        // Two in-window candidates → the newest wins and the pick is flagged
        // ambiguous, regardless of slice order.
        for matches in [
            vec![older.clone(), newer.clone()],
            vec![newer.clone(), older.clone()],
        ] {
            let corr = pick_correlation(&matches, true);
            assert_eq!(corr.session_id, "bbbb-b", "newest chosen");
            assert!(corr.ambiguous, ">1 in-window candidate is ambiguous");
        }
    }

    // peekNewer's created-at ordering + the codex-only filename tiebreak
    // (rollout names are timestamp-prefixed; claude UUID names must NOT
    // tiebreak).
    #[test]
    fn peek_newer_tiebreak() {
        let base = t0();
        let a = cand("/r/rollout-2026-07-11T17-00-05-aaa.jsonl", "a", Some(base));
        let b = cand("/r/rollout-2026-07-11T17-00-01-bbb.jsonl", "b", Some(base));

        // Distinct created-at: time decides, tiebreak irrelevant.
        let newer = cand("/r/x.jsonl", "x", Some(plus(base, Duration::from_secs(1))));
        assert!(peek_newer(&newer, &a, false));
        assert!(!peek_newer(&a, &newer, true));

        // Equal created-at: only the codex flavor breaks the tie by basename.
        assert!(
            peek_newer(&a, &b, true),
            "lexically-later rollout name wins"
        );
        assert!(!peek_newer(&b, &a, true));
        assert!(!peek_newer(&a, &b, false), "no tiebreak without the flag");
        assert!(!peek_newer(&b, &a, false));
    }

    #[test]
    fn within_window_edges() {
        let base = t0();
        let w = CORRELATE_WINDOW;
        assert!(within_window(base, base, w));
        assert!(
            within_window(base, plus(base, w), w),
            "inclusive at the edge"
        );
        assert!(within_window(plus(base, w), base, w), "symmetric");
        assert!(!within_window(
            base,
            plus(base, w + Duration::from_secs(1)),
            w
        ));
    }

    #[test]
    fn parse_jsonl_time_cases() {
        assert_eq!(parse_jsonl_time(""), None);
        assert_eq!(parse_jsonl_time("not a time"), None);
        assert_eq!(parse_jsonl_time("2026-07-11"), None, "date-only is invalid");
        let want = Utc.with_ymd_and_hms(2026, 7, 11, 17, 17, 35).unwrap();
        assert_eq!(parse_jsonl_time("2026-07-11T17:17:35Z"), Some(want));
        // Nano fraction + offset both accepted (Go RFC3339Nano).
        assert_eq!(
            parse_jsonl_time("2026-07-11T17:17:35.123456789Z").map(|t| t.timestamp()),
            Some(want.timestamp())
        );
        assert_eq!(parse_jsonl_time("2026-07-11T19:17:35+02:00"), Some(want));
    }

    /// Records `set-environment` and answers `show-environment` from a map —
    /// the Go suite's `envRecRunner` (`watch_test.go:1169`).
    struct EnvRecRunner {
        env: Mutex<HashMap<String, String>>,
    }

    impl TmuxRunner for EnvRecRunner {
        fn run(&self, args: &[&str]) -> TmuxResult {
            match args.first().copied() {
                Some("set-environment") => {
                    // set-environment -t <name> <KEY> <VAL>
                    if args.len() >= 5 {
                        self.env
                            .lock()
                            .unwrap()
                            .insert(args[3].to_string(), args[4].to_string());
                    }
                    TmuxResult::default()
                }
                Some("show-environment") => {
                    let env = self.env.lock().unwrap();
                    let mut out = String::new();
                    for (k, v) in env.iter() {
                        out.push_str(&format!("{k}={v}\n"));
                    }
                    TmuxResult {
                        stdout: out,
                        ..TmuxResult::default()
                    }
                }
                _ => TmuxResult::default(),
            }
        }
    }

    // Mirrors TestBackWriteAgentSessionRoundTrip.
    #[test]
    fn back_write_agent_session_round_trip() {
        let runner = EnvRecRunner {
            env: Mutex::new(HashMap::new()),
        };
        let tmux = Tmux::new(&runner);
        assert_eq!(agent_session_env(&tmux, "rc-x"), "", "initially unset");
        back_write_agent_session(&tmux, "rc-x", "sess-123");
        assert_eq!(agent_session_env(&tmux, "rc-x"), "sess-123");
        // Control chars are rejected (never stamped); the empty id likewise.
        back_write_agent_session(&tmux, "rc-x", "bad\nvalue");
        back_write_agent_session(&tmux, "rc-x", "");
        assert_eq!(agent_session_env(&tmux, "rc-x"), "sess-123");
    }

    // ---- fileWatcher (H7) ----

    use super::super::watch_codex::CodexFold;

    fn write_file(path: &Path, content: &str) {
        std::fs::write(path, content).expect("write");
    }

    /// Appends to an existing JSONL fixture — the "new content lands after X"
    /// half of the watcher scenarios.
    fn append_file(path: &Path, content: &str) {
        let mut f = std::fs::OpenOptions::new()
            .append(true)
            .open(path)
            .expect("open");
        std::io::Write::write_all(&mut f, content.as_bytes()).expect("append");
    }

    // Mirrors TestFileWatcherFreshnessSettledVsWorkingGrace
    // (watch_test.go:968): settled stays authoritative; working keeps its
    // authority through the LONG grace and only past it demotes to
    // expired_working.
    #[test]
    fn file_watcher_freshness_settled_vs_working_grace() {
        let dir = tempfile::tempdir().expect("tempdir");
        let now = t0();

        let settled_path = dir.path().join("settled.jsonl");
        write_file(
            &settled_path,
            concat!(
                r#"{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"done"}}"#,
                "\n"
            ),
        );
        let sw = FileWatcher::new(
            settled_path.to_str().unwrap(),
            true,
            Box::new(CodexFold::new()),
        );
        sw.refresh(now);
        let (a, msg, fresh, _) = sw.snapshot(now);
        assert_eq!(
            (a, msg.as_str(), fresh),
            (RcActivity::NeedsInput, "done", true)
        );
        let (_, _, fresh, _) = sw.snapshot(plus(now, Duration::from_secs(600)));
        assert!(
            fresh,
            "a settled verdict stays fresh while the file is quiet"
        );

        let work_path = dir.path().join("work.jsonl");
        write_file(
            &work_path,
            concat!(
                r#"{"type":"event_msg","payload":{"type":"task_started"}}"#,
                "\n"
            ),
        );
        let ww = FileWatcher::new(
            work_path.to_str().unwrap(),
            true,
            Box::new(CodexFold::new()),
        );
        ww.refresh(now);
        let (a, _, fresh, expired) = ww.snapshot(now);
        assert_eq!((a, fresh, expired), (RcActivity::Working, true, false));
        let (_, _, fresh, expired) =
            ww.snapshot(plus(now, WATCHER_FRESH_WINDOW + Duration::from_secs(1)));
        assert!(fresh && !expired, "working inside the grace stays fresh");
        let (_, _, fresh, expired) =
            ww.snapshot(plus(now, WATCHER_WORKING_GRACE + Duration::from_secs(1)));
        assert!(
            !fresh && expired,
            "working past the grace is expired_working"
        );
    }

    // Mirrors TestFileWatcherClosedRefreshNoop (hub_messages_test.go:455): a
    // closed watcher's refresh must not reopen the file, refold history, or
    // produce feed messages.
    #[test]
    fn file_watcher_closed_refresh_noop() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("rollout.jsonl");
        write_file(
            &path,
            concat!(
                r#"{"type":"event_msg","payload":{"type":"task_started"}}"#,
                "\n"
            ),
        );
        let w = FileWatcher::new(path.to_str().unwrap(), true, Box::new(CodexFold::new()));
        let now = t0();
        w.refresh(now);
        assert_eq!(w.snapshot(now).0, RcActivity::Working, "precondition");

        w.close();

        // New content lands after close; a refresh must ignore it entirely.
        append_file(
            &path,
            concat!(
                r#"{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"done"}}"#,
                "\n"
            ),
        );
        let later = plus(now, Duration::from_secs(1));
        w.refresh(later);
        assert_eq!(
            w.snapshot(later).0,
            RcActivity::Working,
            "closed watcher folded new lines"
        );
        assert!(
            !w.tailer_is_open(),
            "closed watcher reopened its file handle"
        );
        assert!(w.drain_pending().is_empty(), "no feed messages after close");
        w.close(); // idempotent
    }

    // Mirrors TestCodexFoldGapClearsPendingThenTaskCompleteSettles
    // (watch_test.go:1335), driven through the REAL file watcher: the tool's
    // oversized output line is skipped, the gap clears the pending call, and
    // task_complete settles.
    #[test]
    fn file_watcher_gap_clears_pending_then_settles() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("rollout.jsonl");
        write_file(
            &path,
            concat!(
                r#"{"type":"event_msg","payload":{"type":"task_started"}}"#,
                "\n",
                r#"{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"c1","name":"exec"}}"#,
                "\n"
            ),
        );
        let w = FileWatcher::new(path.to_str().unwrap(), true, Box::new(CodexFold::new()));
        let now = t0();
        w.refresh(now);
        assert_eq!(w.snapshot(now).0, RcActivity::Working, "open tool call");

        let oversized = format!(
            "{}{}{}\n{}\n",
            r#"{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"c1","output":""#,
            "x".repeat(super::super::tail::TAIL_MAX_LINE + 16),
            r#""}}"#,
            r#"{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"done"}}"#,
        );
        append_file(&path, &oversized);
        let later = plus(now, Duration::from_secs(1));
        w.refresh(later);
        assert_eq!(
            w.snapshot(later).0,
            RcActivity::NeedsInput,
            "the gap cleared the pending call"
        );
    }

    // ---- fsNudger (H7) ----

    // Mirrors TestFSNudgerForgetDirAllowsReAdd (watch_test.go:1526): a
    // forgotten parent takes its children out of the added set, and a
    // recreation at the same path can be re-added.
    #[test]
    fn fs_nudger_forget_dir_allows_re_add() {
        let (tx, _rx) = std::sync::mpsc::channel::<notify::Result<notify::Event>>();
        let Ok(watcher) = notify::recommended_watcher(tx) else {
            return; // fsnotify unavailable on this platform: skip, like Go
        };
        let mut state = NudgerState {
            watcher,
            added: std::collections::HashSet::new(),
        };
        let dir = tempfile::tempdir().expect("tempdir");
        let sub = dir.path().join("child");
        std::fs::create_dir_all(&sub).expect("mkdir");
        state.add_dir(dir.path());
        state.add_dir(&sub);
        assert!(
            state.contains(dir.path()) && state.contains(&sub),
            "precondition: both dirs recorded as added"
        );

        // Forgetting the parent must drop it AND its children.
        state.forget_dir(dir.path());
        assert!(
            !state.contains(dir.path()) && !state.contains(&sub),
            "forget_dir left entries behind"
        );

        // A recreation at the same path can now be re-added.
        state.add_dir(dir.path());
        assert!(
            state.contains(dir.path()),
            "re-add after forget must succeed"
        );
    }

    // Mirrors TestFSNudgerNudgesOnChange (watch_test.go:1569): a dated subdir
    // created AFTER the nudger starts must still be watched (the Create
    // handler adds it), and a file write under it nudges.
    #[test]
    fn fs_nudger_nudges_on_change() {
        let root = tempfile::tempdir().expect("tempdir");
        let Ok(mut nudger) =
            FsNudger::new(&[root.path().to_str().unwrap().to_string()], noop_logf())
        else {
            return; // backend unavailable: skip, like Go
        };
        // The root watch itself is already registered — FsNudger::new adds it
        // synchronously, before the loop thread spawns. This waits for the
        // BACKEND event stream (FSEvents in particular) to actually come up,
        // which is the whole reason that add is hoisted out of the thread.
        std::thread::sleep(std::time::Duration::from_millis(50));
        let sub = root.path().join("2026").join("07").join("11");
        std::fs::create_dir_all(&sub).expect("mkdir");
        std::fs::write(sub.join("rollout-x.jsonl"), "hi\n").expect("write");

        nudger
            .nudge()
            .recv_timeout(std::time::Duration::from_secs(3))
            .expect("expected a nudge on a file change under a watched tree");
        nudger.stop();
    }

    // The static-dispatch guard for Go's runtime messageProducer type-assert
    // (H7 review): every fold that implements MessageProducer must ALSO
    // forward drain_fold_messages, or its feed silently vanishes through the
    // trait object (Go's runtime assert cannot be forgotten).
    #[test]
    fn message_producer_folds_forward_through_the_trait_object() {
        // codex
        let mut codex: Box<dyn ActivityFold + Send> =
            Box::new(super::super::watch_codex::CodexFold::new());
        codex.apply_line(
            br#"{"type":"event_msg","payload":{"type":"user_message","message":"hi"}}"#,
        );
        assert_eq!(
            codex.drain_fold_messages().len(),
            1,
            "codex feed reaches the trait object"
        );
        // opencode
        let mut oc: Box<dyn ActivityFold + Send> =
            Box::new(super::super::watch_opencode::OpencodeFold::new());
        oc.apply_line(
            br#"{"type":"permission.asked","properties":{"id":"per_1","sessionID":"s","permission":"bash","patterns":["ls"]}}"#,
        );
        assert_eq!(
            oc.drain_fold_messages().len(),
            1,
            "opencode feed reaches the trait object"
        );
        // claude is activity-only: the default empty drain is correct.
        let mut claude: Box<dyn ActivityFold + Send> =
            Box::new(super::super::watch_claude::ClaudeFold::new());
        claude.apply_line(br#"{"type":"user","message":{"role":"user","content":"hi"}}"#);
        assert!(
            claude.drain_fold_messages().is_empty(),
            "claude contributes no feed rows"
        );
    }

    // The rename-INTO-the-tree arm (H7 review, MEDIUM): notify reports a
    // rename in either direction as Modify(Name(_)) where Go's fsnotify
    // reports rename-in as CREATE — the stat decides, so a directory moved
    // into a watched tree still gets watched and its writes nudge.
    #[test]
    fn fs_nudger_watches_a_dir_renamed_in() {
        let staging = tempfile::tempdir().expect("tempdir");
        let root = tempfile::tempdir().expect("tempdir");
        let staged = staging.path().join("staged");
        std::fs::create_dir_all(&staged).expect("mkdir");
        let Ok(mut nudger) =
            FsNudger::new(&[root.path().to_str().unwrap().to_string()], noop_logf())
        else {
            return; // backend unavailable: skip, like Go
        };
        std::thread::sleep(std::time::Duration::from_millis(100));
        let moved = root.path().join("moved");
        std::fs::rename(&staged, &moved).expect("rename in");
        // Drain the rename's own nudge(s), then prove the moved dir is
        // WATCHED: a write inside it must nudge again.
        std::thread::sleep(std::time::Duration::from_millis(300));
        while nudger.nudge().try_recv().is_ok() {}
        std::fs::write(
            moved.join("rollout-y.jsonl"),
            "hi
",
        )
        .expect("write");
        nudger
            .nudge()
            .recv_timeout(std::time::Duration::from_secs(3))
            .expect("a write inside a renamed-in dir must nudge");
        nudger.stop();
    }

    // Mirrors TestOpencodePortEnv.
    #[test]
    fn opencode_port_env_cases() {
        let cases: [(&str, Option<u16>); 6] = [
            ("4096", Some(4096)),
            ("", None), // the key is never set
            ("abc", None),
            ("0", None),
            ("70000", None),
            ("65535", Some(65535)),
        ];
        for (raw, want) in cases {
            let runner = EnvRecRunner {
                env: Mutex::new(HashMap::new()),
            };
            if !raw.is_empty() {
                runner
                    .env
                    .lock()
                    .unwrap()
                    .insert(ENV_OPENCODE_PORT.to_string(), raw.to_string());
            }
            let tmux = Tmux::new(&runner);
            assert_eq!(opencode_port_env(&tmux, "rc-x"), want, "raw={raw:?}");
        }
    }
}
