package rc

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeClock is a manually-advanced clock for the stability engine tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// scriptedTracker wires a StabilityTracker to a mutable pane variable and a fake
// clock so a test can script snapshot sequences and time.
func scriptedTracker(kind Kind, quiet time.Duration) (*StabilityTracker, *string, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	var pane string
	tr := NewStabilityTracker(kind, func() (string, error) { return pane, nil }, clk.now, quiet)
	return tr, &pane, clk
}

func mustTick(t *testing.T, tr *StabilityTracker) Activity {
	t.Helper()
	a, err := tr.Tick()
	if err != nil {
		t.Fatalf("unexpected Tick error: %v", err)
	}
	return a
}

func TestStabilityChangingIsWorking(t *testing.T) {
	tr, pane, clk := scriptedTracker(KindShell, 4*time.Second)

	*pane = "frame A"
	if a := mustTick(t, tr); a != ActivityWorking {
		t.Fatalf("first capture = %q, want working", a)
	}
	// A changed pane keeps reading working even after long elapsed time — the quiet
	// timer resets on every change.
	clk.advance(30 * time.Second)
	*pane = "frame B — new output"
	if a := mustTick(t, tr); a != ActivityWorking {
		t.Fatalf("changed pane = %q, want working", a)
	}
	clk.advance(30 * time.Second)
	*pane = "frame C — more output"
	if a := mustTick(t, tr); a != ActivityWorking {
		t.Fatalf("still-changing pane = %q, want working", a)
	}
}

func TestStabilityQuietIsIdle(t *testing.T) {
	// KindShell's anchor is the shed PS1 prompt; a non-prompt pane never matches it,
	// so a quiet pane reads idle rather than needs_input.
	tr, pane, clk := scriptedTracker(KindShell, 4*time.Second)

	*pane = "the agent printed a result and stopped"
	if a := mustTick(t, tr); a != ActivityWorking {
		t.Fatalf("first capture = %q, want working", a)
	}
	// Unchanged but not yet quiet-long: still working.
	clk.advance(2 * time.Second)
	if a := mustTick(t, tr); a != ActivityWorking {
		t.Fatalf("stable <quiet = %q, want working (not yet quiet)", a)
	}
	// Crossing the quiet threshold flips to idle.
	clk.advance(3 * time.Second) // total 5s >= 4s
	if a := mustTick(t, tr); a != ActivityIdle {
		t.Fatalf("stable >=quiet = %q, want idle", a)
	}
}

func TestStabilityQuietAtPromptIsNeedsInput(t *testing.T) {
	tr, pane, clk := scriptedTracker(KindCursor, 4*time.Second)

	// A stable pane sitting at cursor's composer placeholder (matches cursorReadyRe).
	*pane = "  Cursor Agent\n\n\n  → Plan, search, build anything\n\n  Auto\n"
	if a := mustTick(t, tr); a != ActivityWorking {
		t.Fatalf("first capture = %q, want working", a)
	}
	clk.advance(5 * time.Second)
	if a := mustTick(t, tr); a != ActivityNeedsInput {
		t.Fatalf("quiet pane at prompt anchor = %q, want needs_input", a)
	}

	// The mid-conversation composer ("→ Add a follow-up") is also an input anchor.
	tr2, pane2, clk2 := scriptedTracker(KindCursor, 4*time.Second)
	*pane2 = "  reply done\n\n  → Add a follow-up\n\n  Auto · 4.9%\n"
	mustTick(t, tr2)
	clk2.advance(5 * time.Second)
	if a := mustTick(t, tr2); a != ActivityNeedsInput {
		t.Fatalf("quiet pane at follow-up anchor = %q, want needs_input", a)
	}
}

func TestStabilitySpinnerOnlyDiffIsIdleAfterQuiet(t *testing.T) {
	// KindShell so the anchor never matches (these frames are not a shed prompt) —
	// the interesting property is that a pane whose ONLY churn is a spinner glyph,
	// an elapsed timer, and ticking token/context/spend readouts still normalizes to
	// a stable snapshot and reads idle.
	tr, pane, clk := scriptedTracker(KindShell, 4*time.Second)

	frame1 := "⠋ Thinking\nBuild · Big Pickle · 3.6s\n8,390 tokens\n4% used\n$0.00 spent\nresult text here"
	frame2 := "⠙ Thinking\nBuild · Big Pickle · 4.1s\n8,450 tokens\n5% used\n$0.01 spent\nresult text here"
	frame3 := "⠹ Thinking\nBuild · Big Pickle · 4.9s\n8,510 tokens\n6% used\n$0.02 spent\nresult text here"

	// Sanity: the raw frames genuinely differ, but normalize identically.
	if frame1 == frame2 {
		t.Fatal("test frames must differ raw")
	}
	if normalizePane(frame1) != normalizePane(frame2) || normalizePane(frame2) != normalizePane(frame3) {
		t.Fatalf("spinner/timer/counter churn survived normalization:\n%q\n%q",
			normalizePane(frame1), normalizePane(frame2))
	}

	*pane = frame1
	if a := mustTick(t, tr); a != ActivityWorking {
		t.Fatalf("first capture = %q, want working", a)
	}
	clk.advance(2 * time.Second)
	*pane = frame2 // raw-different, normalized-identical → still stable, not yet quiet
	if a := mustTick(t, tr); a != ActivityWorking {
		t.Fatalf("normalized-stable <quiet = %q, want working", a)
	}
	clk.advance(3 * time.Second) // total quiet duration crossed
	*pane = frame3
	if a := mustTick(t, tr); a != ActivityIdle {
		t.Fatalf("spinner-only churn after quiet = %q, want idle", a)
	}
}

func TestNormalizeDurationTokenSubstitution(t *testing.T) {
	t.Run("changing duration in stable text is not churn", func(t *testing.T) {
		a := normalizePane("tests passed in 3.6s — all green")
		b := normalizePane("tests passed in 4.1s — all green")
		if a != b {
			t.Fatalf("duration tick churned the diff:\n%q\n%q", a, b)
		}
		// The surrounding real content must survive substitution, not vanish.
		if !strings.Contains(a, "tests passed in") || !strings.Contains(a, "all green") {
			t.Fatalf("real content vanished from the normalized line: %q", a)
		}
	})

	t.Run("clock-style elapsed time is not churn", func(t *testing.T) {
		if normalizePane("elapsed 0:12:45 · running") != normalizePane("elapsed 0:12:46 · running") {
			t.Fatal("clock tick churned the diff")
		}
	})

	t.Run("footer gauge tick is not churn", func(t *testing.T) {
		a := normalizePane("  8.4K (4%)  ctrl+p commands")
		b := normalizePane("  8.5K (5%)  ctrl+p commands")
		if a != b {
			t.Fatalf("gauge tick churned the diff:\n%q\n%q", a, b)
		}
		if !strings.Contains(a, "ctrl+p commands") {
			t.Fatalf("footer chrome vanished: %q", a)
		}
	})

	t.Run("a NEW line containing a duration registers as change", func(t *testing.T) {
		before := normalizePane("agent output")
		after := normalizePane("agent output\ntest suite took 3s")
		if before == after {
			t.Fatal("new duration-bearing line was invisible to the diff (line-drop regression)")
		}
		// And the line's non-duration text is present in the normalized form.
		if !strings.Contains(after, "test suite took") {
			t.Fatalf("duration-bearing content line vanished: %q", after)
		}
	})

	t.Run("pure telemetry chrome lines still drop whole", func(t *testing.T) {
		for _, line := range []string{"8,390 tokens", "  4% used", "$0.00 spent", "1.2K tokens"} {
			if got := normalizePane("real content\n" + line); got != "real content" {
				t.Errorf("pure chrome line %q not dropped: %q", line, got)
			}
		}
		// But a chrome-shaped phrase inside a content line is NOT a full-line match.
		if got := normalizePane("the model used 8,390 tokens for this"); !strings.Contains(got, "for this") {
			t.Errorf("content line swallowed by chrome filter: %q", got)
		}
	})
}

func TestStabilityNewDurationLineIsWorking(t *testing.T) {
	// Engine-level version of the new-line case: a quiet session that PRINTS a new
	// line containing a duration must flip back to working — token substitution keeps
	// the line in the diff, unlike the old whole-line drop.
	tr, pane, clk := scriptedTracker(KindShell, 4*time.Second)
	*pane = "agent output"
	mustTick(t, tr)
	clk.advance(5 * time.Second)
	if a := mustTick(t, tr); a != ActivityIdle {
		t.Fatalf("stable pane = %q, want idle", a)
	}
	*pane = "agent output\ntest suite took 3s"
	if a := mustTick(t, tr); a != ActivityWorking {
		t.Fatalf("new duration-bearing line = %q, want working", a)
	}
}

func TestStabilityEmptyPane(t *testing.T) {
	tr, pane, clk := scriptedTracker(KindOpencode, 4*time.Second)

	*pane = "" // blank pane (just-started / cleared)
	if a := mustTick(t, tr); a != ActivityWorking {
		t.Fatalf("first empty capture = %q, want working", a)
	}
	clk.advance(5 * time.Second)
	if a := mustTick(t, tr); a != ActivityIdle {
		t.Fatalf("stable empty pane = %q, want idle (no anchor match)", a)
	}
}

func TestStabilityCaptureError(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	sentinel := errors.New("tmux session gone")
	tr := NewStabilityTracker(KindShell, func() (string, error) { return "", sentinel }, clk.now, 4*time.Second)

	a, err := tr.Tick()
	if !errors.Is(err, sentinel) {
		t.Fatalf("Tick err = %v, want sentinel", err)
	}
	if a != ActivityUnknown {
		t.Fatalf("Tick on capture error = %q, want unknown", a)
	}
}

func TestStabilityDefaultQuietPeriod(t *testing.T) {
	// quiet <= 0 falls back to DefaultQuietPeriod.
	tr, pane, clk := scriptedTracker(KindShell, 0)
	*pane = "static"
	mustTick(t, tr)
	clk.advance(DefaultQuietPeriod - time.Millisecond)
	if a := mustTick(t, tr); a != ActivityWorking {
		t.Fatalf("just under default quiet = %q, want working", a)
	}
	clk.advance(2 * time.Millisecond)
	if a := mustTick(t, tr); a != ActivityIdle {
		t.Fatalf("past default quiet = %q, want idle", a)
	}
}
