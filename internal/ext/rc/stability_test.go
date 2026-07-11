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

// stabilityStep scripts one Tick: advance the clock, optionally replace the pane,
// then Tick and assert the derived Activity.
type stabilityStep struct {
	advance time.Duration
	pane    string
	setPane bool
	want    Activity
}

// TestStabilityScenarios is the table-driven engine behavior suite: each case scripts
// a pane/clock sequence for a kind and asserts the Activity at every Tick. Focused
// tests (TestStabilityCaptureError, TestNormalizeDurationTokenSubstitution,
// TestNormalizeSpinnerChurn) cover error and normalization-specific behavior.
func TestStabilityScenarios(t *testing.T) {
	const (
		cursorPlaceholder = "  Cursor Agent\n\n\n  → Plan, search, build anything\n\n  Auto\n"
		cursorFollowUp    = "  reply done\n\n  → Add a follow-up\n\n  Auto · 4.9%\n"
		spinner1          = "⠋ Thinking\nBuild · Big Pickle · 3.6s\n8,390 tokens\n4% used\n$0.00 spent\nresult text here"
		spinner2          = "⠙ Thinking\nBuild · Big Pickle · 4.1s\n8,450 tokens\n5% used\n$0.01 spent\nresult text here"
		spinner3          = "⠹ Thinking\nBuild · Big Pickle · 4.9s\n8,510 tokens\n6% used\n$0.02 spent\nresult text here"
	)
	cases := []struct {
		name  string
		kind  Kind
		quiet time.Duration
		steps []stabilityStep
	}{
		{
			name:  "changing pane stays working despite long elapsed time",
			kind:  KindShell,
			quiet: 4 * time.Second,
			steps: []stabilityStep{
				{pane: "frame A", setPane: true, want: ActivityWorking},
				// A changed pane keeps reading working even after long elapsed time —
				// the quiet timer resets on every change.
				{advance: 30 * time.Second, pane: "frame B — new output", setPane: true, want: ActivityWorking},
				{advance: 30 * time.Second, pane: "frame C — more output", setPane: true, want: ActivityWorking},
			},
		},
		{
			// KindShell's anchor is the shed PS1 prompt; a non-prompt pane never matches
			// it, so a quiet pane reads idle rather than needs_input.
			name:  "quiet non-prompt pane is idle",
			kind:  KindShell,
			quiet: 4 * time.Second,
			steps: []stabilityStep{
				{pane: "the agent printed a result and stopped", setPane: true, want: ActivityWorking},
				{advance: 2 * time.Second, want: ActivityWorking}, // stable, not yet quiet
				{advance: 3 * time.Second, want: ActivityIdle},    // total 5s >= 4s
			},
		},
		{
			// A stable pane at cursor's composer placeholder matches cursorReadyRe.
			name:  "quiet cursor placeholder anchor is needs_input",
			kind:  KindCursor,
			quiet: 4 * time.Second,
			steps: []stabilityStep{
				{pane: cursorPlaceholder, setPane: true, want: ActivityWorking},
				{advance: 5 * time.Second, want: ActivityNeedsInput},
			},
		},
		{
			// The mid-conversation composer ("→ Add a follow-up") is also an anchor.
			name:  "quiet cursor follow-up anchor is needs_input",
			kind:  KindCursor,
			quiet: 4 * time.Second,
			steps: []stabilityStep{
				{pane: cursorFollowUp, setPane: true, want: ActivityWorking},
				{advance: 5 * time.Second, want: ActivityNeedsInput},
			},
		},
		{
			// A pane whose only churn is a spinner glyph, an elapsed timer, and ticking
			// token/context/spend readouts normalizes to a stable snapshot and reads idle.
			name:  "spinner-only churn is idle after quiet",
			kind:  KindShell,
			quiet: 4 * time.Second,
			steps: []stabilityStep{
				{pane: spinner1, setPane: true, want: ActivityWorking},
				// raw-different, normalized-identical → still stable, not yet quiet.
				{advance: 2 * time.Second, pane: spinner2, setPane: true, want: ActivityWorking},
				{advance: 3 * time.Second, pane: spinner3, setPane: true, want: ActivityIdle},
			},
		},
		{
			// A quiet session that PRINTS a new line containing a duration must flip back
			// to working — token substitution keeps the line in the diff.
			name:  "new duration-bearing line flips back to working",
			kind:  KindShell,
			quiet: 4 * time.Second,
			steps: []stabilityStep{
				{pane: "agent output", setPane: true, want: ActivityWorking},
				{advance: 5 * time.Second, want: ActivityIdle},
				{pane: "agent output\ntest suite took 3s", setPane: true, want: ActivityWorking},
			},
		},
		{
			name:  "empty pane with no anchor is idle when quiet",
			kind:  KindOpencode,
			quiet: 4 * time.Second,
			steps: []stabilityStep{
				{pane: "", setPane: true, want: ActivityWorking}, // blank pane (just-started / cleared)
				{advance: 5 * time.Second, want: ActivityIdle},
			},
		},
		{
			// quiet <= 0 falls back to DefaultQuietPeriod.
			name:  "quiet<=0 falls back to DefaultQuietPeriod",
			kind:  KindShell,
			quiet: 0,
			steps: []stabilityStep{
				{pane: "static", setPane: true, want: ActivityWorking},
				{advance: DefaultQuietPeriod - time.Millisecond, want: ActivityWorking},
				{advance: 2 * time.Millisecond, want: ActivityIdle},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, pane, clk := scriptedTracker(tc.kind, tc.quiet)
			for i, s := range tc.steps {
				if s.advance > 0 {
					clk.advance(s.advance)
				}
				if s.setPane {
					*pane = s.pane
				}
				if a := mustTick(t, tr); a != s.want {
					t.Fatalf("step %d: activity = %q, want %q", i, a, s.want)
				}
			}
		})
	}
}

// TestNormalizeSpinnerChurn is the focused normalization property behind the
// spinner-only stability scenario: raw-different spinner/timer/counter frames must
// normalize identically so the pane reads stable.
func TestNormalizeSpinnerChurn(t *testing.T) {
	frame1 := "⠋ Thinking\nBuild · Big Pickle · 3.6s\n8,390 tokens\n4% used\n$0.00 spent\nresult text here"
	frame2 := "⠙ Thinking\nBuild · Big Pickle · 4.1s\n8,450 tokens\n5% used\n$0.01 spent\nresult text here"
	frame3 := "⠹ Thinking\nBuild · Big Pickle · 4.9s\n8,510 tokens\n6% used\n$0.02 spent\nresult text here"
	if frame1 == frame2 {
		t.Fatal("test frames must differ raw")
	}
	if normalizePane(frame1) != normalizePane(frame2) || normalizePane(frame2) != normalizePane(frame3) {
		t.Fatalf("spinner/timer/counter churn survived normalization:\n%q\n%q",
			normalizePane(frame1), normalizePane(frame2))
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
