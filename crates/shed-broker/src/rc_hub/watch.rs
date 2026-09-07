//! Watcher contracts: the freshness rule, the `mergedActivity` precedence
//! merge, and the tmux env seams — the pure parts of
//! `internal/ext/rc/watch.go` (plan 010 H4).
//!
//! A structured-signal watcher OVERRIDES the pane stability engine for the
//! kinds that have one: instead of inferring activity from whether the tmux
//! pane keeps redrawing, it reads the agent's own turn/tool structure directly.
//! opencode is the one such kind — its watcher subscribes to the agent's
//! embedded HTTP+SSE server. The hub merges a session's watcher with pane
//! stability per session: a fresh, correlated watcher wins; a broken/absent one
//! falls back to stability so activity never goes dark.
//!
//! The codex JSONL tail, the cursor hook-ingest push lane and the shared line
//! tailer they both sat on were removed with A6 (`charliek/shed#322`); the
//! correlation helpers (`Correlation`/`JsonlPeek`/`pick_correlation`/…) and
//! `list_jsonl_under` went with them, since only those two lanes ever mapped a
//! tmux session to a file on disk.
//!
//! It also carries the fold contracts ([`ActivityFold`] / [`MessageProducer`] —
//! Go's `activityFold`/`messageProducer` interfaces, `watch.go`), consumed by
//! [`super::watch_opencode`], and the `notify`-backed [`FsNudger`].

use std::path::Path;
use std::time::Duration;

use chrono::{DateTime, Utc};
use shed_core::rc::{RcActivity, RcKind};
use shed_core::rc_agents::{has_control_chars, parse_env, ENV_AGENT_SESSION, ENV_OPENCODE_PORT};
use shed_rc_engine::tmux::Tmux;

use super::messages::FeedMessage;

/// Folds a kind's parsed line stream into a live activity verdict
/// (`activityFold`, `watch.go`). Implementations hold cumulative state
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
    /// [`MessageProducer::drain_messages`]; an activity-only fold with no feed
    /// inherits the empty default and contributes no feed rows.
    fn drain_fold_messages(&mut self) -> Vec<FeedMessage> {
        Vec::new()
    }
}

/// The narrow surface the reconcile loop and the input handler need from a
/// per-session watcher (`sessionWatcher`, `watch.go:120`): refresh it, read
/// its current verdict, drain any feed messages it produced, and check
/// whether it has ever folded an event. Implemented by [`FileWatcher`]
/// (codex), the cursor watcher, and (H8) the opencode watcher, so
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
    /// Go's `confirmedAgentIDDrainer` type-assert
    /// (`watch_opencode_transport.go:126`).
    fn as_confirmed_agent_id_drainer(&self) -> Option<&dyn ConfirmedAgentIdDrainer> {
        None
    }
    /// Go's `approvalPublisher` type-assert (`watch_opencode_transport.go:135`).
    fn as_approval_publisher(&self) -> Option<&dyn ApprovalPublisher> {
        None
    }
    /// The claim seam ([`ClaimHolder`]) — opencode only; every other lane
    /// owns its conversation by construction.
    fn as_claim_holder(&self) -> Option<&dyn ClaimHolder> {
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

/// A stream-discovered agent session id awaiting reconcile's
/// `SHED_RC_AGENT_SESSION` back-write (`confirmedAgentIDDrainer`,
/// `watch_opencode_transport.go:126`) — so a hub restart re-correlates
/// exactly. Implemented by the opencode watcher.
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
/// `watch.go`) — opencode today. Every watcher drains it on each refresh. It is
/// a separate trait from [`ActivityFold`] so a fold can produce a feed without
/// being an `ActivityFold` at all.
pub trait MessageProducer {
    /// Returns and clears the feed messages produced since the last drain.
    fn drain_messages(&mut self) -> Vec<FeedMessage>;
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

// The file-watcher transport (`fileWatcher`, watch.go) lived here: the
// resilient JSONL line tailer paired with an [`ActivityFold`] behind the
// [`SessionWatcher`] contract. Both it and the tailer it wrapped served only
// the codex rollout lane, and went with that lane in A6 (`charliek/shed#322`).
// The contract survives — the opencode transport implements it.

// ---------------------------------------------------------------------------
// fsnotify nudge layer (watch.go:438-564) over the `notify` crate
// ---------------------------------------------------------------------------// ---------------------------------------------------------------------------
// fsnotify nudge layer (watch.go:438-564) over the `notify` crate
// ---------------------------------------------------------------------------

/// Watches a set of root trees and pings a channel whenever a file changes, so
/// the hub can run a reconcile sub-tick (`fsNudger`, `watch.go:447`) — activity
/// surfaces promptly instead of waiting up to the active interval. It is a
/// best-effort LATENCY optimization: the reconcile tick already refreshes every
/// watcher, so a missed notification only delays a transition to the next tick.
/// Watching is non-recursive, so directories are added as they appear.
///
/// No kind tails a file since A6 (`charliek/shed#322`), so the hub builds it
/// over an EMPTY root set today and the tick is the sole driver; the seam stays
/// for the next file-backed lane, and the tests below drive it directly.
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
    use chrono::{DateTime, Utc};

    /// The non-blank lines of a shared JSONL fixture (`crates/fixtures/jsonl`).
    pub(crate) fn fixture_lines(name: &str) -> Vec<Vec<u8>> {
        let path = format!("{}/../fixtures/jsonl/{name}", env!("CARGO_MANIFEST_DIR"));
        let data = std::fs::read(&path).expect("fixture readable");
        data.split(|&b| b == b'\n')
            .filter(|l| !l.iter().all(u8::is_ascii_whitespace))
            .map(<[u8]>::to_vec)
            .collect()
    }

    /// The WATCHER suites' clock origin: any fixed instant works, since every
    /// watcher assertion is relative to it.
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

/// THE quiet-source freshness rule (`watcherFreshness`, `watch.go:227`),
/// shared verbatim by every watcher that has one (the opencode watcher once
/// its transport is healthy). Given a verdict, whether it is settled, and when
/// the source last produced an event, it reports the verdict's authority at
/// `now`:
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
/// `watch.go:303`). opencode is the only one: it subscribes to its embedded
/// HTTP+SSE server. Every other kind derives activity from pane stability alone
/// — A6 (`charliek/shed#322`) retired the codex rollout tail and the cursor
/// hook-ingest lane, and A5 (`charliek/shed#321`) the claude transcript tail
/// before them.
pub fn watchable_kind(k: &RcKind) -> bool {
    matches!(k, RcKind::Opencode)
}

// The file-correlation helpers (`Correlation`, `JsonlPeek`, `PeekCandidate`,
// `peek_newer`, `pick_correlation`, `within_window`, `parse_jsonl_time`) lived
// here. Only the codex rollout lane ever mapped a tmux session to a file on
// disk, so they went with that lane in A6 (`charliek/shed#322`). opencode
// correlates over its own SSE stream and needs none of it.

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
        assert!(watchable_kind(&RcKind::Opencode));
        assert!(
            !watchable_kind(&RcKind::Codex),
            "codex no longer tails a rollout (shed#322)"
        );
        assert!(
            !watchable_kind(&RcKind::Cursor),
            "cursor no longer has a hook-ingest lane (shed#322)"
        );
        assert!(
            !watchable_kind(&RcKind::ClaudeRc),
            "claude no longer tails a transcript (shed#321)"
        );
        assert!(!watchable_kind(&RcKind::ClaudeBroker));
        assert!(!watchable_kind(&RcKind::Shell), "shell is stability only");
        assert!(!watchable_kind(&RcKind::Other("mystery".into())));
    }

    // The correlation-helper cells (`pick_correlation` newest/ambiguity,
    // `peek_newer` tiebreak, `within_window` edges, `parse_jsonl_time` cases)
    // went with the codex rollout lane in A6 (`charliek/shed#322`), together
    // with the helpers themselves.

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

    // The `FileWatcher` cells (settled-vs-working-grace freshness, the
    // closed-refresh no-op, the gap-clears-pending fold) went with the codex
    // rollout lane and its tailer in A6 (`charliek/shed#322`). The freshness
    // rule they drove through that transport survives above
    // (`freshness_settled_vs_working_grace`) and on the opencode transport.

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
