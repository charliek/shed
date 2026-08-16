package rc

import (
	"strings"
	"testing"
	"time"
)

// cursor's pane-anchor approvals + its newly gated input (plan 008 §3.5/§3.6). cursor has
// no approval hook event at all, so the pane is the only evidence its TUI is blocked on
// the operator — the same mechanism codex uses, over cursor's own widget chrome.

// The anchor's positives and negatives, straight off the committed fixtures. The
// post-resolution fixture is the reason the anchor is a CONJUNCTION: the headline "Run
// this command?" is still on screen after the prompt is answered (it scrolled into the
// transcript), and only the option rows go away.
func TestCursorApprovalAnchorFixtures(t *testing.T) {
	anchor := approvalAnchorFor(KindCursor)
	if anchor == nil {
		t.Fatal("cursor must declare an ApprovalAnchor")
	}
	cases := []struct {
		fixture string
		want    bool
	}{
		{"cursor-ready-approval-shell", true},
		{"cursor-ready-approval-hook", true},   // selection arrowed onto the LAST row
		{"cursor-ready-approval-delete", true}, // the one-word label set (Delete/Keep)
		{"cursor-ready-approval-write", true},  // no auto-run row; a dynamic Add Write(…) label
		{"cursor-ready-approval-fetch", true},  // a dynamic Always allow <domain> label
		{"cursor-ready-approval-resolved", false},
		{"cursor-ready-approval-quoted", false},
		{"cursor-ready", false},
		{"cursor-ready-active", false},
		{"cursor-needs-auth", false},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			pane := paneFixture(t, c.fixture)
			if got := anchor.MatchString(pane); got != c.want {
				t.Errorf("anchor match = %v, want %v", got, c.want)
			}
			if c.want {
				// The row's summary is the widget's headline line — what the operator is
				// being asked, not the whole matched span.
				line := firstAnchorLine(anchor, pane)
				if !strings.HasSuffix(line, "?") && !strings.HasPrefix(line, "Hook requested approval:") {
					t.Errorf("firstAnchorLine = %q, want the prompt's headline line", line)
				}
			}
		})
	}
}

// EXHAUSTIVENESS over cursor's decision surfaces is a SAFETY property, not a nicety: a
// surface whose rows do not match opens no episode, and the input gate would then be one
// expired-working verdict away from typing into the widget (a posted "y" answers it). This
// pins every headline decision-logic.ts can render against a representative option row, so
// a cursor release that adds a surface fails HERE rather than in production.
func TestCursorApprovalAnchorCoversEveryDecisionSurface(t *testing.T) {
	anchor := approvalAnchorFor(KindCursor)
	surfaces := []struct{ name, headline, option string }{
		{"shell", "Run this command?", "   → Run (once) (y)"},
		{"shell/sandbox", "Run this command outside the sandbox?", "   → Run outside sandbox (once) (y)"},
		{"shell/allowlist", "Run this command?", "     Add Shell(npm test) to allowlist? (tab)"},
		{"shell/autorun", "Run this command?", "     Run Everything (shift+tab)"},
		{"shell/sandbox-autorun", "Run this command?", "     Run in Sandbox (shift+tab)"},
		{"shell/autorun-loading", "Run this command?", "     Checking Run Everything availability (loading)"},
		{"mcp", "Run this MCP tool?", "     Allowlist MCP Tool (tab)"},
		{"mcp/skip", "Run this MCP tool?", "     Skip (esc or n)"},
		{"delete", "Delete this file?", "   → Delete (y)"},
		{"delete/keep", "Delete this file?", "     Keep (n)"},
		{"write", "Write to this file?", "   → Proceed (y)"},
		{"write/reject", "Write to this file?", "     Reject & propose changes (esc or n or p)"},
		{"write/allowlist", "Write to this file?", "     Add Write(/tmp/x.toml) to allowlist? (tab)"},
		{"write/allowlist-generic", "Write to this file?", "     Add to allowlist (tab)"},
		{"search", "Allow this web search?", "   → Allow search (y)"},
		{"fetch", "Allow this web fetch?", "   → Fetch (y)"},
		{"fetch/always", "Allow this web fetch?", "     Always allow example.com (tab)"},
		{"edit", "Proceed with this edit?", "   → Proceed (y)"},
		{"hook", "Hook requested approval: policy said no", "   → Run (once) (y)"},
	}
	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			pane := " " + s.headline + "\n\n" + s.option + "\n"
			if !anchor.MatchString(pane) {
				t.Errorf("the %s surface does not match the anchor:\n%s", s.name, pane)
			}
		})
	}
}

// Neither half of the conjunction is trusted alone: a headline gets quoted in prose and
// survives the answer, and an option label is a phrase that can end a line of prose.
func TestCursorApprovalAnchorNeedsBothHalves(t *testing.T) {
	anchor := approvalAnchorFor(KindCursor)
	const headline = " Run this command?"
	const optionRow = "   → Run (once) (y)"
	if !strings.Contains(paneFixture(t, "cursor-ready-approval-resolved"), "Run this command?") {
		t.Fatal("test premise: the post-resolution fixture must still carry the headline")
	}
	if anchor.MatchString(headline + "\nsome unrelated line\n") {
		t.Error("the headline alone must never match")
	}
	if anchor.MatchString("blah\n" + optionRow + "\n") {
		t.Error("an option row alone must never match")
	}
	// Markdown bullets are how an assistant lists the labels — never an accepted gutter.
	if anchor.MatchString(" Run this command?\n - Run (once)\n") {
		t.Error("a bulleted label must not satisfy the option-row half")
	}
	// The real widget: both halves, in order.
	if !anchor.MatchString(headline + "\n Not in allowlist: ls\n\n" + optionRow + "\n") {
		t.Error("headline + option row must match")
	}
}

// The debounced pane-approval episode (the C3 mechanism) applies to cursor unchanged:
// two matching ticks to open, two clearing ticks to close, one informational row each way,
// and `needs_approval` on the wire in between.
func TestCursorPaneApprovalEpisode(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)
	f.set("rc-cap001", paneFixture(t, "cursor-ready-approval-shell"), managedEnv("id-cap", KindCursor))

	h.reconcile() // tick 1: matched once — not yet debounced
	if got := hubActivityOf(t, h, "cap001"); got == ActivityNeedsApproval {
		t.Fatal("a single matching tick must not flip activity")
	}
	h.reconcile() // tick 2: debounced detection
	if got := hubActivityOf(t, h, "cap001"); got != ActivityNeedsApproval {
		t.Fatalf("activity = %q, want needs_approval after two matching ticks", got)
	}
	pend := hubPendingApprovals(t, h, "cap001")
	if len(pend) != 1 || pend[0].ID != "pane-1" || pend[0].Status != approvalStatusPending {
		t.Fatalf("pending_approvals = %+v, want one pending pane-1", pend)
	}
	// approvals stays "tui" for cursor: the row advertises NO decisions, because there is
	// nothing the hub could honor remotely.
	if len(pend[0].Decisions) != 0 {
		t.Errorf("a pane-derived approval must advertise no decisions: %+v", pend[0])
	}

	f.setPane("rc-cap001", paneFixture(t, "cursor-ready-approval-resolved"))
	h.reconcile()
	h.reconcile() // debounced clear
	if got := hubActivityOf(t, h, "cap001"); got == ActivityNeedsApproval {
		t.Error("two clearing ticks must drop needs_approval")
	}
	if pend := hubPendingApprovals(t, h, "cap001"); len(pend) != 0 {
		t.Errorf("pending_approvals = %+v, want empty after the clear", pend)
	}
}

// THE INPUT GATE for the newly-gated cursor kind: a ready composer accepts, and the same
// session with the approval widget on the VISIBLE frame refuses — a posted line would
// otherwise be typed straight into the prompt and answer it by accident.
func TestCursorInputGateAcceptsReadyRejectsApproval(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)

	if kindFeatureRow(KindCursor).Input != inputModeGated {
		t.Fatal("test premise: cursor's row must advertise gated feed input")
	}
	settled := &stubWatcher{activity: ActivityNeedsInput, fresh: true}
	ready := paneFixture(t, "cursor-ready")
	approval := paneFixture(t, "cursor-ready-approval-shell")

	if !h.inputAccepted(settled, ActivityNeedsInput, KindCursor, ready, ready) {
		t.Error("a ready cursor composer must accept feed input")
	}
	if h.inputAccepted(settled, ActivityNeedsInput, KindCursor, approval, approval) {
		t.Error("an approval prompt on the visible frame must reject input")
	}
	// The approval fixture still matches the READY/prompt anchor (cursor keeps its composer
	// drawn, disabled, under the decision surface) — which is exactly why the approval arm
	// has to exist: without it the degraded anchor path would have accepted.
	if !promptAnchorFor(KindCursor).MatchString(approval) {
		t.Fatal("test premise: the approval fixture still shows the composer anchor")
	}
	// Scrollback is not evidence about the present: a prompt answered long ago sits in the
	// history forever, and gating on it would wedge the session's input.
	if !h.inputAccepted(settled, ActivityNeedsInput, KindCursor, approval, ready) {
		t.Error("an approval prompt present only in the scrollback must not gate input")
	}
	// Post-resolution and quoted prose both flow again.
	for _, fx := range []string{"cursor-ready-approval-resolved", "cursor-ready-approval-quoted"} {
		if !h.inputAccepted(settled, ActivityNeedsInput, KindCursor, paneFixture(t, fx), paneFixture(t, fx)) {
			t.Errorf("%s: input must be accepted", fx)
		}
	}
}

// AgentSpec.ComposerUnderModal is a RENDERING FACT, and the fixtures are what make it one:
// codex's overlay replaces the composer (its prompt anchor cannot match a pane showing the
// dialog), cursor's is drawn disabled underneath (its prompt anchor still matches). The
// input gate's expired-working arm is derived from this flag, so a wrong value here is a
// silent hole — pin it against the panes.
func TestComposerUnderModalMatchesTheFixtures(t *testing.T) {
	cases := []struct {
		kind    Kind
		fixture string
	}{
		{KindCodex, "codex-ready-approval-exec"},
		{KindCodex, "codex-ready-approval-network"},
		{KindCursor, "cursor-ready-approval-shell"},
		{KindCursor, "cursor-ready-approval-delete"},
		{KindCursor, "cursor-ready-approval-write"},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			anchor := promptAnchorFor(c.kind)
			if anchor == nil {
				t.Fatalf("%s must declare a PromptAnchor", c.kind)
			}
			composerVisible := anchor.MatchString(paneFixture(t, c.fixture))
			if want := composerUnderModal(c.kind); composerVisible != want {
				t.Errorf("the composer is %v on a %s approval pane, but ComposerUnderModal = %v",
					composerVisible, c.kind, want)
			}
		})
	}
}

// THE HOLE THE FLAG CLOSES: a cursor approval surface the anchor does not recognize, with
// the operator away long enough for the working verdict to expire and the frozen pane to
// settle. Without the expired-working arm the degraded path sees cursor's still-drawn
// composer and ACCEPTS — and the posted text would be typed into the widget, where a "y"
// answers it. Simulated with an anchor-invisible pane, which is precisely what an
// unrecognized widget looks like to the gate.
func TestCursorInputGateRejectsExpiredWorkingUnderAnUnknownModal(t *testing.T) {
	f := newHubTmux()
	start := time.Unix(1_700_000_000, 0).UTC()
	clk := &hubClock{t: start}
	h := newTestHub(f, clk)

	// A cursor turn in flight (the hook stream's last word), then the operator walks away:
	// no `stop` ever arrives, so the verdict expires.
	w := newCursorWatcher("", nil)
	w.pushHookEvent(hookEv("preToolUse", `{"session_id":"`+cursorTestSessionID+`","tool_name":"Delete","tool_input":{"file_path":"/home/shed/proj/build.json"}}`))
	w.refresh(clk.now())
	clk.advance(watcherWorkingGrace + time.Second)
	if _, _, fresh, expired := w.snapshot(clk.now()); fresh || !expired {
		t.Fatalf("premise: the verdict must be expired-working (fresh=%v expired=%v)", fresh, expired)
	}

	// The pane the gate sees: a widget the anchor does not know, with cursor's composer
	// still drawn beneath it, and a stability verdict of idle (the frozen pane stopped
	// changing) — the exact combination that used to accept.
	unknownModal := paneFixture(t, "cursor-ready") + "\n Some future approval widget?\n   → Yes, do it (y)\n"
	if approvalAnchorFor(KindCursor).MatchString(unknownModal) {
		t.Fatal("premise: this pane must NOT match the approval anchor")
	}
	if !promptAnchorFor(KindCursor).MatchString(unknownModal) {
		t.Fatal("premise: cursor's composer must still be visible under the widget")
	}
	if h.inputAccepted(w, ActivityIdle, KindCursor, unknownModal, unknownModal) {
		t.Error("an expired-working cursor verdict must never accept via the composer anchor")
	}

	// The legitimate case is unaffected: a turn that genuinely ended emits `stop`, which
	// settles the fold — and a settled verdict accepts, however long it has been quiet.
	w.pushHookEvent(hookEv("stop", `{"session_id":"`+cursorTestSessionID+`","status":"completed"}`))
	w.refresh(clk.now())
	clk.advance(24 * time.Hour)
	if !h.inputAccepted(w, ActivityIdle, KindCursor, paneFixture(t, "cursor-ready"), paneFixture(t, "cursor-ready")) {
		t.Error("a settled cursor verdict must still accept input")
	}

	// And a cursor session with NO watcher verdict at all (hooks never arrived — the
	// preseed was skipped by the mount guard) keeps the degraded anchor path: refusing
	// there would make the kind unsteerable in a configuration the guard deliberately
	// creates.
	if !h.inputAccepted(nil, ActivityIdle, KindCursor, paneFixture(t, "cursor-ready"), paneFixture(t, "cursor-ready")) {
		t.Error("with no watcher verdict the composer anchor must still accept")
	}
}

// The hook watcher's own verdict still governs the gate: a working cursor session refuses
// input even though its composer anchor is on the pane (delivering mid-turn would
// interleave a line into a running turn).
func TestCursorInputGateRejectsWhileWorking(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)

	w := newCursorWatcher("", nil)
	w.pushHookEvent(hookEv("beforeSubmitPrompt", `{"session_id":"`+cursorTestSessionID+`","prompt":"go"}`))
	w.refresh(clk.now())
	if h.inputAccepted(w, ActivityNeedsInput, KindCursor, paneFixture(t, "cursor-ready"), paneFixture(t, "cursor-ready")) {
		t.Error("a working hook verdict must reject input even at the composer")
	}

	// …and accepts once `stop` lands (the settled boundary).
	w.pushHookEvent(hookEv("stop", `{"session_id":"`+cursorTestSessionID+`","status":"completed"}`))
	w.refresh(clk.now())
	if !h.inputAccepted(w, ActivityIdle, KindCursor, paneFixture(t, "cursor-ready"), paneFixture(t, "cursor-ready")) {
		t.Error("a settled hook verdict must accept input")
	}
}
