package rc

import (
	"regexp"
	"strings"
	"time"
)

// The pane-stability engine is the universal activity fallback: for kinds with no
// structured signal (opencode/cursor/shell) — and as a backstop when a codex/claude
// JSONL tail breaks — activity is derived purely from whether the tmux pane keeps
// changing. A changing pane means the agent is producing output (working); a pane
// that holds still for a quiet period means the agent finished (idle), or is parked
// at its input composer (needs_input, when the kind declares a PromptAnchor the
// stable pane matches).
//
// The signal is noisy: a pane "changes" every frame just from a spinner glyph
// rotating or an elapsed-time / token-counter readout ticking, even when the agent is
// otherwise done. So a snapshot is NORMALIZED (those volatile elements filtered out)
// before it is diffed — a pane whose only churn is a spinner or a timer normalizes to
// a stable string and correctly reads idle.
//
// Everything here is pure and injectable: the tracker takes a capture func and a
// clock, so tests drive it with synthetic snapshot sequences and a fake clock — no
// tmux, no real time.

// CapturePaneFunc returns the current pane text for the tracked session. An error
// (e.g. the tmux session vanished) is surfaced by Tick unchanged.
type CapturePaneFunc func() (string, error)

// DefaultQuietPeriod is how long a normalized pane must hold still before the engine
// downgrades working → idle/needs_input. 4s is long enough to ride over a brief pause
// between tool calls (so a mid-turn gap is not misread as "done") yet short enough
// that a finished session flips within a poll or two. Injectable per tracker.
const DefaultQuietPeriod = 4 * time.Second

// StabilityTracker holds one session's pane-stability state across Tick calls. Not
// safe for concurrent use — one tracker per watched session, driven from a single
// goroutine. Construct with NewStabilityTracker.
type StabilityTracker struct {
	capture CapturePaneFunc
	now     func() time.Time
	quiet   time.Duration
	anchor  *regexp.Regexp // kind's prompt anchor; nil ⇒ stable always reads idle

	hasPrev     bool
	prevNorm    string
	stableSince time.Time
	activity    Activity
}

// NewStabilityTracker builds a tracker for a kind. capture supplies pane text, now is
// the clock (both injectable for tests), and quiet is the stable-period threshold;
// quiet <= 0 falls back to DefaultQuietPeriod. The kind's PromptAnchor is resolved
// from the registry — an anchorless kind never reports needs_input (stable ⇒ idle).
func NewStabilityTracker(kind Kind, capture CapturePaneFunc, now func() time.Time, quiet time.Duration) *StabilityTracker {
	if quiet <= 0 {
		quiet = DefaultQuietPeriod
	}
	if now == nil {
		now = time.Now
	}
	return &StabilityTracker{
		capture: capture,
		now:     now,
		quiet:   quiet,
		anchor:  promptAnchorFor(kind),
	}
}

// Tick captures the pane, normalizes it, and updates the derived activity:
//
//   - normalized snapshot differs from the previous ⇒ working (and the quiet timer
//     resets to now);
//   - snapshot unchanged AND it has held for >= the quiet period ⇒ needs_input when
//     the pane matches the kind's prompt anchor, else idle;
//   - snapshot unchanged but not yet quiet-long ⇒ the prior activity is retained
//     (working until the quiet period actually elapses).
//
// The first Tick always reports working (a fresh session has "just changed"). A
// capture error is returned verbatim with ActivityUnknown and leaves the tracker
// state untouched (the next successful Tick resumes from the last good snapshot).
func (t *StabilityTracker) Tick() (Activity, error) {
	pane, err := t.capture()
	if err != nil {
		return ActivityUnknown, err
	}
	norm := normalizePane(pane)
	now := t.now()

	if !t.hasPrev || norm != t.prevNorm {
		t.hasPrev = true
		t.prevNorm = norm
		t.stableSince = now
		t.activity = ActivityWorking
		return t.activity, nil
	}

	// Unchanged since last capture. Downgrade only once the pane has been quiet for
	// the full period; before that, hold the prior activity (typically working).
	if now.Sub(t.stableSince) >= t.quiet {
		if t.anchor != nil && t.anchor.MatchString(pane) {
			t.activity = ActivityNeedsInput
		} else {
			t.activity = ActivityIdle
		}
	}
	return t.activity, nil
}

// Volatile-content filters for normalizePane. Two mechanisms, deliberately distinct:
//
//   - TOKEN SUBSTITUTION (volatileTokenRes): a ticking readout embedded in a line
//     with real content — an elapsed timer, a clock, a context gauge — is replaced
//     in place by a fixed placeholder. The readout stops churning the diff, but the
//     REST of the line still participates: real output like "test suite took 3s"
//     never vanishes, and a NEW line that happens to contain a duration still
//     registers as a change.
//   - LINE DROP (volatileLineRes): a line that is PURELY telemetry chrome (nothing
//     on it but a counter/gauge/spend readout) is removed whole. These are anchored
//     full-line matches so they can never swallow a content line.
//
// Kept as a small, documented regex table so a new TUI's spinner/telemetry style can
// be added in one place.
var (
	// spinnerGlyphRe matches the braille and box/quadrant glyphs coding-agent TUIs
	// cycle for a "thinking" spinner. Includes the plan's canonical braille frames
	// (⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏) and the heavy-braille (⣾⣽⣻⢿⡿⣟⣯⣷) and box/quadrant
	// (▘▝▀▖▗▚▞▙▛▜▟▐▌) families. Stripped inline so a pane that differs ONLY by which
	// frame is showing normalizes identically frame-to-frame.
	spinnerGlyphRe = regexp.MustCompile(`[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏⣾⣽⣻⢿⡿⣟⣯⣷▘▝▀▖▗▚▞▙▛▜▟▐▌]`)

	// volatileTokenRes are within-line substitutions: each match is replaced by
	// volatileTokenPlaceholder so its ticking value can't churn the diff while the
	// surrounding text stays live.
	volatileTokenRes = []*regexp.Regexp{
		// Elapsed duration: "3.6s", "131ms", "· 4s" ("Build · Big Pickle · 3.6s",
		// "Thought: 131ms", "esc to interrupt · 4s").
		regexp.MustCompile(`(?i)\b\d+(?:\.\d+)?\s*m?s\b`),
		// Clock-style elapsed time: "1:23", "0:12:45".
		regexp.MustCompile(`\b\d+:\d{2}(?::\d{2})?\b`),
		// Context gauge embedded in footer chrome: "8.4K (4%)" in opencode's
		// "8.4K (4%)  ctrl+p commands" status line.
		regexp.MustCompile(`(?i)\b[\d.,]+\s*[km]?\s*\(\d+%\)`),
	}

	// volatileLineRes drop a line that is nothing but a live-updating readout
	// (token counter, context-percent gauge, spend meter — the opencode sidebar
	// style). Anchored `^…$` so a line with any other content is kept.
	volatileLineRes = []*regexp.Regexp{
		// Token counter line: "8,390 tokens", "1.2K tokens".
		regexp.MustCompile(`(?i)^\s*[\d.,]+\s*[km]?\s*tokens?\s*$`),
		// Context-percent line: "4% used", "12% context".
		regexp.MustCompile(`(?i)^\s*\d+%\s*(?:used|context)\s*$`),
		// Spend meter line: "$0.00 spent".
		regexp.MustCompile(`(?i)^\s*\$[\d.,]+\s*spent\s*$`),
	}
)

// volatileTokenPlaceholder replaces every volatileTokenRes match. Any fixed string
// works (it exists only to make successive normalized snapshots comparable); kept
// short and unlikely to appear in real pane text.
const volatileTokenPlaceholder = "‹t›"

// normalizePane produces the diff-stable form of a captured pane: strip spinner
// glyphs inline, drop pure-telemetry lines whole, substitute ticking tokens
// (timers/clocks/gauges) with a fixed placeholder, trim trailing whitespace per line
// (a redraw often shifts trailing padding), and rejoin. Two captures of the same
// quiescent screen — differing only in spinner frame or a ticking readout —
// normalize to the same string, which is what lets the engine call them stable.
func normalizePane(pane string) string {
	lines := strings.Split(pane, "\n")
	out := lines[:0]
	for _, line := range lines {
		line = spinnerGlyphRe.ReplaceAllString(line, "")
		if isVolatileLine(line) {
			continue
		}
		for _, re := range volatileTokenRes {
			line = re.ReplaceAllString(line, volatileTokenPlaceholder)
		}
		out = append(out, strings.TrimRight(line, " \t"))
	}
	return strings.Join(out, "\n")
}

// isVolatileLine reports whether a line is purely a live-updating readout that should
// be dropped before diffing (see volatileLineRes).
func isVolatileLine(line string) bool {
	if strings.TrimSpace(line) == "" {
		return false
	}
	for _, re := range volatileLineRes {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}
