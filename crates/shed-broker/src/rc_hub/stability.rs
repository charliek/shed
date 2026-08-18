//! The pane-stability engine — a port of `internal/ext/rc/stability.go`.
//!
//! The universal activity fallback: for kinds with no structured signal
//! (opencode/cursor/shell) — and as a backstop when a codex/claude JSONL tail
//! breaks — activity is derived purely from whether the tmux pane keeps
//! changing. A changing pane means the agent is producing output (working); a
//! pane that holds still for a quiet period means the agent finished (idle),
//! or is parked at its input composer (needs_input, when the kind declares a
//! PromptAnchor the stable pane matches).
//!
//! The signal is noisy: a pane "changes" every frame just from a spinner glyph
//! rotating or an elapsed-time / token-counter readout ticking, even when the
//! agent is otherwise done. So a snapshot is NORMALIZED (those volatile
//! elements filtered out) before it is diffed — a pane whose only churn is a
//! spinner or a timer normalizes to a stable string and correctly reads idle.
//!
//! Everything here is pure and injectable: the tracker takes a capture fn and
//! a clock, so tests drive it with synthetic snapshot sequences and a fake
//! clock — no tmux, no real time.

use std::sync::LazyLock;
use std::time::Duration;

use chrono::{DateTime, Utc};
use regex::Regex;
use shed_core::rc::{RcActivity, RcKind};
use shed_core::rc_agents::prompt_anchor_for;

/// Returns the current pane text for the tracked session; the error (e.g. the
/// tmux session vanished) is surfaced by [`StabilityTracker::tick`] unchanged
/// (`CapturePaneFunc`, `stability.go:29`).
pub type CapturePaneFn = Box<dyn FnMut() -> Result<String, String> + Send>;

/// The injectable clock (`now func() time.Time` on the Go tracker).
pub type NowFn = Box<dyn FnMut() -> DateTime<Utc> + Send>;

/// How long a normalized pane must hold still before the engine downgrades
/// working → idle/needs_input (`DefaultQuietPeriod`, `stability.go:35`). 4s is
/// long enough to ride over a brief pause between tool calls (so a mid-turn
/// gap is not misread as "done") yet short enough that a finished session
/// flips within a poll or two. Injectable per tracker.
pub const DEFAULT_QUIET_PERIOD: Duration = Duration::from_secs(4);

/// Go's `\s` class, written out — Go RE2's Perl classes are ASCII-only where
/// Rust's default to Unicode (same discipline as `shed_core::rc_agents`).
/// `\b` gets the same treatment HERE, as `(?-u:\b)`: unlike the classifier
/// anchors (where `\b` divergence needs a non-ASCII letter directly adjacent
/// to an anchored English word — unreachable), the volatile-token substitutions
/// run against arbitrary agent output, where a readout abutting a CJK/accented
/// character ("中4s", "é1:23") is realistic — a Unicode `\b` would refuse the
/// match Go makes and let the readout churn the diff forever (H4 review
/// finding, proven by a 6.4k-input Go↔Rust differential).
const GO_SPACE: &str = r"[\t\n\f\r ]";

/// One session's pane-stability state across [`StabilityTracker::tick`] calls
/// (`StabilityTracker`, `stability.go:40`). Not safe for concurrent use — one
/// tracker per watched session, driven from a single thread (the reconcile
/// loop). Construct with [`StabilityTracker::new`].
pub struct StabilityTracker {
    capture: CapturePaneFn,
    now: NowFn,
    quiet: Duration,
    /// The kind's prompt anchor; `None` ⇒ stable always reads idle.
    anchor: Option<&'static Regex>,

    has_prev: bool,
    prev_norm: String,
    stable_since: DateTime<Utc>,
    activity: RcActivity,
}

impl StabilityTracker {
    /// Builds a tracker for a kind (`NewStabilityTracker`, `stability.go:56`).
    /// `capture` supplies pane text, `now` is the clock (both injectable for
    /// tests), and `quiet` is the stable-period threshold; a zero `quiet`
    /// falls back to [`DEFAULT_QUIET_PERIOD`] (Go's `<= 0` — a `Duration` here
    /// cannot be negative). The kind's PromptAnchor is resolved from the
    /// registry — an anchorless kind never reports needs_input (stable ⇒ idle).
    pub fn new(
        kind: &RcKind,
        capture: CapturePaneFn,
        now: NowFn,
        quiet: Duration,
    ) -> StabilityTracker {
        let quiet = if quiet.is_zero() {
            DEFAULT_QUIET_PERIOD
        } else {
            quiet
        };
        StabilityTracker {
            capture,
            now,
            quiet,
            anchor: prompt_anchor_for(kind),
            has_prev: false,
            prev_norm: String::new(),
            stable_since: DateTime::<Utc>::MIN_UTC,
            activity: RcActivity::Unknown,
        }
    }

    /// Captures the pane, normalizes it, and updates the derived activity
    /// (`(*StabilityTracker).Tick`, `stability.go:83`):
    ///
    /// - normalized snapshot differs from the previous ⇒ working (and the
    ///   quiet timer resets to now);
    /// - snapshot unchanged AND it has held for >= the quiet period ⇒
    ///   needs_input when the pane matches the kind's prompt anchor, else idle;
    /// - snapshot unchanged but not yet quiet-long ⇒ the prior activity is
    ///   retained (working until the quiet period actually elapses).
    ///
    /// The first tick always reports working (a fresh session has "just
    /// changed"). A capture error is returned verbatim (Go's
    /// `(ActivityUnknown, err)` pair maps to `Err`) and leaves the tracker
    /// state untouched — the next successful tick resumes from the last good
    /// snapshot.
    pub fn tick(&mut self) -> Result<RcActivity, String> {
        let pane = (self.capture)()?;
        let norm = normalize_pane(&pane);
        let now = (self.now)();

        if !self.has_prev || norm != self.prev_norm {
            self.has_prev = true;
            self.prev_norm = norm;
            self.stable_since = now;
            self.activity = RcActivity::Working;
            return Ok(self.activity);
        }

        // Unchanged since last capture. Downgrade only once the pane has been
        // quiet for the full period; before that, hold the prior activity
        // (typically working). A clock that went backwards reads as "not yet
        // quiet" (Go's negative Sub compares < quiet the same way).
        let held = now
            .signed_duration_since(self.stable_since)
            .to_std()
            .unwrap_or(Duration::ZERO);
        if held >= self.quiet {
            self.activity = match self.anchor {
                Some(anchor) if anchor.is_match(&pane) => RcActivity::NeedsInput,
                _ => RcActivity::Idle,
            };
        }
        Ok(self.activity)
    }
}

// Volatile-content filters for normalize_pane (`stability.go:111-158`). Two
// mechanisms, deliberately distinct:
//
//   - TOKEN SUBSTITUTION (VOLATILE_TOKEN_RES): a ticking readout embedded in a
//     line with real content — an elapsed timer, a clock, a context gauge — is
//     replaced in place by a fixed placeholder. The readout stops churning the
//     diff, but the REST of the line still participates: real output like
//     "test suite took 3s" never vanishes, and a NEW line that happens to
//     contain a duration still registers as a change.
//   - LINE DROP (VOLATILE_LINE_RES): a line that is PURELY telemetry chrome
//     (nothing on it but a counter/gauge/spend readout) is removed whole.
//     These are anchored full-line matches so they can never swallow a content
//     line.
//
// Kept as a small, documented regex table so a new TUI's spinner/telemetry
// style can be added in one place.

/// The braille and box/quadrant glyphs coding-agent TUIs cycle for a
/// "thinking" spinner (`spinnerGlyphRe`, `stability.go:131`). Stripped inline
/// so a pane that differs ONLY by which frame is showing normalizes
/// identically frame-to-frame.
static SPINNER_GLYPH_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new("[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏⣾⣽⣻⢿⡿⣟⣯⣷▘▝▀▖▗▚▞▙▛▜▟▐▌]").expect("spinner glyph regex compiles")
});

/// Within-line substitutions: each match is replaced by
/// [`VOLATILE_TOKEN_PLACEHOLDER`] so its ticking value can't churn the diff
/// while the surrounding text stays live (`volatileTokenRes`,
/// `stability.go:136`).
static VOLATILE_TOKEN_RES: LazyLock<[Regex; 3]> = LazyLock::new(|| {
    [
        // Elapsed duration: "3.6s", "131ms", "· 4s".
        Regex::new(&format!(
            r"(?i)(?-u:\b)[0-9]+(?:\.[0-9]+)?{GO_SPACE}*m?s(?-u:\b)"
        ))
        .expect("duration regex"),
        // Clock-style elapsed time: "1:23", "0:12:45".
        Regex::new(r"(?-u:\b)[0-9]+:[0-9]{2}(?::[0-9]{2})?(?-u:\b)").expect("clock regex"),
        // Context gauge embedded in footer chrome: "8.4K (4%)" in opencode's
        // "8.4K (4%)  ctrl+p commands" status line.
        Regex::new(&format!(
            r"(?i)(?-u:\b)[0-9.,]+{GO_SPACE}*[km]?{GO_SPACE}*\([0-9]+%\)"
        ))
        .expect("gauge regex"),
    ]
});

/// Full-line drops for a line that is nothing but a live-updating readout
/// (token counter, context-percent gauge, spend meter — the opencode sidebar
/// style). Anchored `^…$` so a line with any other content is kept
/// (`volatileLineRes`, `stability.go:150`).
static VOLATILE_LINE_RES: LazyLock<[Regex; 3]> = LazyLock::new(|| {
    [
        // Token counter line: "8,390 tokens", "1.2K tokens".
        Regex::new(&format!(
            r"(?i)^{GO_SPACE}*[0-9.,]+{GO_SPACE}*[km]?{GO_SPACE}*tokens?{GO_SPACE}*$"
        ))
        .expect("token counter regex"),
        // Context-percent line: "4% used", "12% context".
        Regex::new(&format!(
            r"(?i)^{GO_SPACE}*[0-9]+%{GO_SPACE}*(?:used|context){GO_SPACE}*$"
        ))
        .expect("context percent regex"),
        // Spend meter line: "$0.00 spent".
        Regex::new(&format!(
            r"(?i)^{GO_SPACE}*\$[0-9.,]+{GO_SPACE}*spent{GO_SPACE}*$"
        ))
        .expect("spend meter regex"),
    ]
});

/// Replaces every [`VOLATILE_TOKEN_RES`] match. Any fixed string works (it
/// exists only to make successive normalized snapshots comparable); kept short
/// and unlikely to appear in real pane text (`volatileTokenPlaceholder`).
const VOLATILE_TOKEN_PLACEHOLDER: &str = "‹t›";

/// Produces the diff-stable form of a captured pane (`normalizePane`,
/// `stability.go:171`): strip spinner glyphs inline, drop pure-telemetry lines
/// whole, substitute ticking tokens (timers/clocks/gauges) with a fixed
/// placeholder, trim trailing whitespace per line (a redraw often shifts
/// trailing padding), and rejoin. Two captures of the same quiescent screen —
/// differing only in spinner frame or a ticking readout — normalize to the
/// same string, which is what lets the engine call them stable.
pub(crate) fn normalize_pane(pane: &str) -> String {
    let mut out: Vec<String> = Vec::new();
    for line in pane.split('\n') {
        let line = SPINNER_GLYPH_RE.replace_all(line, "");
        if is_volatile_line(&line) {
            continue;
        }
        let mut line = line.into_owned();
        for re in VOLATILE_TOKEN_RES.iter() {
            line = re
                .replace_all(&line, VOLATILE_TOKEN_PLACEHOLDER)
                .into_owned();
        }
        out.push(line.trim_end_matches([' ', '\t']).to_string());
    }
    out.join("\n")
}

/// Whether a line is purely a live-updating readout that should be dropped
/// before diffing (`isVolatileLine`, `stability.go:189`).
fn is_volatile_line(line: &str) -> bool {
    if line.trim().is_empty() {
        return false;
    }
    VOLATILE_LINE_RES.iter().any(|re| re.is_match(line))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{Arc, Mutex};

    /// A manually-advanced clock + a shared pane variable, wiring a scripted
    /// tracker exactly like Go's `scriptedTracker`
    /// (`stability_test.go:11-23`).
    struct Script {
        pane: Arc<Mutex<String>>,
        clock: Arc<Mutex<DateTime<Utc>>>,
    }

    fn scripted_tracker(kind: &RcKind, quiet: Duration) -> (StabilityTracker, Script) {
        let pane = Arc::new(Mutex::new(String::new()));
        let clock = Arc::new(Mutex::new(
            DateTime::from_timestamp(1_700_000_000, 0).expect("valid epoch"),
        ));
        let (p, c) = (Arc::clone(&pane), Arc::clone(&clock));
        let tracker = StabilityTracker::new(
            kind,
            Box::new(move || Ok(p.lock().unwrap().clone())),
            Box::new(move || *c.lock().unwrap()),
            quiet,
        );
        (tracker, Script { pane, clock })
    }

    impl Script {
        fn advance(&self, d: Duration) {
            let mut t = self.clock.lock().unwrap();
            *t += chrono::Duration::from_std(d).expect("in range");
        }
        fn set_pane(&self, s: &str) {
            *self.pane.lock().unwrap() = s.to_string();
        }
    }

    struct Step {
        advance: Duration,
        pane: Option<&'static str>,
        want: RcActivity,
    }

    fn step(advance: Duration, pane: Option<&'static str>, want: RcActivity) -> Step {
        Step {
            advance,
            pane,
            want,
        }
    }

    // Mirrors TestStabilityScenarios — the table-driven engine behavior suite:
    // each case scripts a pane/clock sequence for a kind and asserts the
    // activity at every tick.
    #[test]
    fn stability_scenarios() {
        const CURSOR_PLACEHOLDER: &str =
            "  Cursor Agent\n\n\n  → Plan, search, build anything\n\n  Auto\n";
        const CURSOR_FOLLOW_UP: &str = "  reply done\n\n  → Add a follow-up\n\n  Auto · 4.9%\n";
        const SPINNER1: &str = "⠋ Thinking\nBuild · Big Pickle · 3.6s\n8,390 tokens\n4% used\n$0.00 spent\nresult text here";
        const SPINNER2: &str = "⠙ Thinking\nBuild · Big Pickle · 4.1s\n8,450 tokens\n5% used\n$0.01 spent\nresult text here";
        const SPINNER3: &str = "⠹ Thinking\nBuild · Big Pickle · 4.9s\n8,510 tokens\n6% used\n$0.02 spent\nresult text here";

        let secs = Duration::from_secs;
        let cases: Vec<(&str, RcKind, Duration, Vec<Step>)> = vec![
            (
                "changing pane stays working despite long elapsed time",
                RcKind::Shell,
                secs(4),
                vec![
                    step(secs(0), Some("frame A"), RcActivity::Working),
                    // A changed pane keeps reading working even after long
                    // elapsed time — the quiet timer resets on every change.
                    step(secs(30), Some("frame B — new output"), RcActivity::Working),
                    step(secs(30), Some("frame C — more output"), RcActivity::Working),
                ],
            ),
            (
                // Shell's anchor is the shed PS1 prompt; a non-prompt pane
                // never matches it, so a quiet pane reads idle.
                "quiet non-prompt pane is idle",
                RcKind::Shell,
                secs(4),
                vec![
                    step(
                        secs(0),
                        Some("the agent printed a result and stopped"),
                        RcActivity::Working,
                    ),
                    step(secs(2), None, RcActivity::Working), // stable, not yet quiet
                    step(secs(3), None, RcActivity::Idle),    // total 5s >= 4s
                ],
            ),
            (
                "quiet cursor placeholder anchor is needs_input",
                RcKind::Cursor,
                secs(4),
                vec![
                    step(secs(0), Some(CURSOR_PLACEHOLDER), RcActivity::Working),
                    step(secs(5), None, RcActivity::NeedsInput),
                ],
            ),
            (
                // The mid-conversation composer ("→ Add a follow-up") is also
                // an anchor.
                "quiet cursor follow-up anchor is needs_input",
                RcKind::Cursor,
                secs(4),
                vec![
                    step(secs(0), Some(CURSOR_FOLLOW_UP), RcActivity::Working),
                    step(secs(5), None, RcActivity::NeedsInput),
                ],
            ),
            (
                // A pane whose only churn is a spinner glyph, an elapsed
                // timer, and ticking token/context/spend readouts normalizes
                // to a stable snapshot and reads idle.
                "spinner-only churn is idle after quiet",
                RcKind::Shell,
                secs(4),
                vec![
                    step(secs(0), Some(SPINNER1), RcActivity::Working),
                    // raw-different, normalized-identical → still stable, not
                    // yet quiet.
                    step(secs(2), Some(SPINNER2), RcActivity::Working),
                    step(secs(3), Some(SPINNER3), RcActivity::Idle),
                ],
            ),
            (
                // A quiet session that PRINTS a new line containing a duration
                // must flip back to working — token substitution keeps the
                // line in the diff.
                "new duration-bearing line flips back to working",
                RcKind::Shell,
                secs(4),
                vec![
                    step(secs(0), Some("agent output"), RcActivity::Working),
                    step(secs(5), None, RcActivity::Idle),
                    step(
                        secs(0),
                        Some("agent output\ntest suite took 3s"),
                        RcActivity::Working,
                    ),
                ],
            ),
            (
                "empty pane with no anchor match is idle when quiet",
                RcKind::Opencode,
                secs(4),
                vec![
                    step(secs(0), Some(""), RcActivity::Working), // blank pane (just-started / cleared)
                    step(secs(5), None, RcActivity::Idle),
                ],
            ),
            (
                // quiet == 0 falls back to DEFAULT_QUIET_PERIOD.
                "quiet zero falls back to the default quiet period",
                RcKind::Shell,
                Duration::ZERO,
                vec![
                    step(secs(0), Some("static"), RcActivity::Working),
                    step(
                        DEFAULT_QUIET_PERIOD - Duration::from_millis(1),
                        None,
                        RcActivity::Working,
                    ),
                    step(Duration::from_millis(2), None, RcActivity::Idle),
                ],
            ),
        ];

        for (name, kind, quiet, steps) in cases {
            let (mut tracker, script) = scripted_tracker(&kind, quiet);
            for (i, s) in steps.iter().enumerate() {
                if s.advance > Duration::ZERO {
                    script.advance(s.advance);
                }
                if let Some(p) = s.pane {
                    script.set_pane(p);
                }
                let got = tracker.tick().expect("tick succeeds");
                assert_eq!(got, s.want, "{name}: step {i}");
            }
        }
    }

    // Mirrors TestNormalizeSpinnerChurn — the focused normalization property
    // behind the spinner-only stability scenario.
    #[test]
    fn normalize_spinner_churn() {
        let frame1 = "⠋ Thinking\nBuild · Big Pickle · 3.6s\n8,390 tokens\n4% used\n$0.00 spent\nresult text here";
        let frame2 = "⠙ Thinking\nBuild · Big Pickle · 4.1s\n8,450 tokens\n5% used\n$0.01 spent\nresult text here";
        let frame3 = "⠹ Thinking\nBuild · Big Pickle · 4.9s\n8,510 tokens\n6% used\n$0.02 spent\nresult text here";
        assert_ne!(frame1, frame2, "test frames must differ raw");
        assert_eq!(normalize_pane(frame1), normalize_pane(frame2));
        assert_eq!(normalize_pane(frame2), normalize_pane(frame3));
    }

    // Mirrors TestNormalizeDurationTokenSubstitution.
    #[test]
    fn normalize_duration_token_substitution() {
        // Changing duration in stable text is not churn — and the surrounding
        // real content survives substitution.
        let a = normalize_pane("tests passed in 3.6s — all green");
        let b = normalize_pane("tests passed in 4.1s — all green");
        assert_eq!(a, b, "duration tick must not churn the diff");
        assert!(a.contains("tests passed in") && a.contains("all green"));

        // Clock-style elapsed time is not churn.
        assert_eq!(
            normalize_pane("elapsed 0:12:45 · running"),
            normalize_pane("elapsed 0:12:46 · running")
        );

        // Footer gauge tick is not churn, and the chrome around it survives.
        let a = normalize_pane("  8.4K (4%)  ctrl+p commands");
        let b = normalize_pane("  8.5K (5%)  ctrl+p commands");
        assert_eq!(a, b, "gauge tick must not churn the diff");
        assert!(a.contains("ctrl+p commands"));

        // A NEW line containing a duration registers as change (line-drop
        // regression guard), and its non-duration text is present.
        let before = normalize_pane("agent output");
        let after = normalize_pane("agent output\ntest suite took 3s");
        assert_ne!(before, after, "new duration-bearing line must be visible");
        assert!(after.contains("test suite took"));

        // Pure telemetry chrome lines still drop whole…
        for line in ["8,390 tokens", "  4% used", "$0.00 spent", "1.2K tokens"] {
            assert_eq!(
                normalize_pane(&format!("real content\n{line}")),
                "real content",
                "pure chrome line {line:?} must drop"
            );
        }
        // …but a chrome-shaped phrase inside a content line is NOT a full-line
        // match.
        assert!(
            normalize_pane("the model used 8,390 tokens for this").contains("for this"),
            "content line must not be swallowed by the chrome filter"
        );
    }

    // The `(?-u:\b)` regression pin (H4 review finding): a ticking readout
    // abutting a non-ASCII word character must still substitute, exactly as
    // Go's ASCII-only `\b` does — a Unicode `\b` refuses these matches and the
    // readout churns the diff forever (the session never settles).
    #[test]
    fn normalize_token_substitution_ascii_boundary() {
        // Accented + CJK neighbours around a duration.
        assert_eq!(normalize_pane("é4s"), "é‹t›");
        assert_eq!(normalize_pane("4sé"), "‹t›é");
        assert_eq!(
            normalize_pane("Build · Big Pickle · 3.6s経過"),
            normalize_pane("Build · Big Pickle · 4.1s経過"),
            "CJK-adjacent duration tick must not churn the diff"
        );
        // Clock-style elapsed time with non-ASCII neighbours substitutes WHOLE.
        assert_eq!(normalize_pane("прошло 0:12:45назад"), "прошло ‹t›назад");
        // A stability arc over CJK frames actually settles.
        let (mut tracker, script) = scripted_tracker(&RcKind::Shell, Duration::from_secs(4));
        script.set_pane("作業中\nBuild · Big Pickle · 3.6s経過\nresult text here");
        assert_eq!(tracker.tick(), Ok(RcActivity::Working));
        script.advance(Duration::from_secs(3));
        script.set_pane("作業中\nBuild · Big Pickle · 4.1s経過\nresult text here");
        assert_eq!(tracker.tick(), Ok(RcActivity::Working));
        script.advance(Duration::from_secs(3));
        script.set_pane("作業中\nBuild · Big Pickle · 4.9s経過\nresult text here");
        assert_eq!(
            tracker.tick(),
            Ok(RcActivity::Idle),
            "a CJK pane whose only churn is the timer must settle"
        );
    }

    // Mirrors TestStabilityCaptureError: a capture error surfaces verbatim
    // (Go's `(ActivityUnknown, err)` is this port's `Err`) and leaves state
    // untouched.
    #[test]
    fn capture_error_surfaces() {
        let mut tracker = StabilityTracker::new(
            &RcKind::Shell,
            Box::new(|| Err("tmux session gone".to_string())),
            Box::new(|| DateTime::from_timestamp(1_700_000_000, 0).expect("valid epoch")),
            Duration::from_secs(4),
        );
        assert_eq!(tracker.tick(), Err("tmux session gone".to_string()));
        assert!(
            !tracker.has_prev,
            "an errored tick must not record a snapshot"
        );
    }
}
