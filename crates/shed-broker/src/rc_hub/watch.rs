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
//! Still Go-only until their commits: `fileWatcher` + `fsNudger` (H7 —
//! transports), the per-kind folds + `correlateCodex`/`correlateClaude`
//! (H5/H6), `listJSONLUnder` (H5, with its correlate consumers).

use std::path::Path;
use std::time::Duration;

use chrono::{DateTime, Utc};
use shed_core::rc::{RcActivity, RcKind};
use shed_core::rc_agents::{has_control_chars, parse_env, ENV_AGENT_SESSION, ENV_OPENCODE_PORT};
use shed_rc_engine::tmux::Tmux;

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
