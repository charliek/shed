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
//! [`super::watch_claude`] / [`super::watch_codex`] / [`super::watch_cursor`].
//! Still Go-only until their commits: `fileWatcher` + `fsNudger` (H7 —
//! transports), the opencode fold (H6) and its SSE transport (H8).

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
/// tool_use's input detail (`compactJSON`, `watch_opencode.go:932`; consumed
/// by the cursor fold now, the opencode fold at H6). Mirrors Go's
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
    T: serde::de::DeserializeOwned,
{
    let raws: Option<Vec<Box<serde_json::value::RawValue>>> = serde::Deserialize::deserialize(d)?;
    let Some(raws) = raws else {
        return Ok(Vec::new());
    };
    raws.into_iter()
        .map(|r| {
            let s = r.get().trim();
            if !s.starts_with('{') {
                return Err(serde::de::Error::custom("expected a JSON object element"));
            }
            serde_json::from_str::<T>(s).map_err(serde::de::Error::custom)
        })
        .collect()
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
    /// `base`).
    pub(crate) fn base_time() -> DateTime<Utc> {
        Utc.with_ymd_and_hms(2026, 7, 11, 17, 0, 0).unwrap()
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
    use super::*;
    use chrono::TimeZone;
    use shed_rc_engine::tmux::{TmuxResult, TmuxRunner};
    use std::collections::HashMap;
    use std::sync::Mutex;

    fn t0() -> DateTime<Utc> {
        DateTime::from_timestamp(1_700_000_000, 0).expect("valid epoch")
    }

    fn plus(base: DateTime<Utc>, d: Duration) -> DateTime<Utc> {
        base + chrono::Duration::from_std(d).expect("in range")
    }

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
