package rc

import (
	"testing"
	"time"
)

// TestTypingMidTurnIsDeliveredBecauseTheTUIQueuesIt pins the capability the rule
// change exists for, against panes captured from LIVE sessions.
//
// The behaviour it rests on was verified by hand on 2026-08-23: a line sent to
// codex while it was mid-turn landed in its composer (the footer offered "tab to
// queue message"), and codex answered it as soon as the running turn finished.
// cursor takes typing mid-turn the same way. So "the agent is busy" is not a
// reason to refuse a line — refusing it was the single biggest reason a person
// could not answer a question they could already see on their phone.
//
// What must STILL refuse is a decision surface, because there a delivered
// sentence does not queue — it answers.
func TestTypingMidTurnIsDeliveredBecauseTheTUIQueuesIt(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)

	for _, tc := range []struct {
		name string
		kind Kind
		pane string
		want bool
	}{
		{"codex working, composer drawn beneath", KindCodex, codexLivePaneWorking, true},
		{"codex settled at its composer", KindCodex, codexLivePaneIdle, true},
		{"cursor working, hint on the composer line", KindCursor, cursorLivePaneWorking, true},
		{"cursor with the operator's own text typed in", KindCursor, cursorLivePaneTyped, true},
		{"cursor blocked on a command approval", KindCursor, cursorLivePaneApproval, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No watcher and an idle stability: the pane is the only evidence,
			// which is the degraded case the old rule was strictest about.
			got := h.inputAccepted(nil, ActivityIdle, tc.kind, tc.pane, tc.pane)
			if got != tc.want {
				t.Fatalf("inputAccepted = %v, want %v", got, tc.want)
			}
		})
	}

	// And the codex dialog captured from the same live session refuses too — its
	// options are numbered and Enter takes the highlighted one.
	codexDialog := paneFixture(t, "codex-ready-approval-exec")
	if h.inputAccepted(nil, ActivityNeedsInput, KindCodex, codexDialog, codexDialog) {
		t.Error("a codex approval dialog must refuse a posted line")
	}
}
