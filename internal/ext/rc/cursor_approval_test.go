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

// The expired-working arm's REJECT side (its guarded-recovery accept side is
// TestCursorInputGateExpiredWorkingIdleComposerRecovers). When the working verdict has
// expired and the fresh visible pane shows no KNOWN approval surface, input is recovered
// only if pane-stability has settled on needs_input; a stability verdict that is anything
// else (idle here — the frozen pane stopped but never reached the composer verdict) still
// rejects. The exhaustive ApprovalAnchor (not a blanket guess) is what keeps a real dialog
// impossible to type into — see TestCursorApprovalAnchorCoversEveryDecisionSurface for the
// exhaustiveness that (a) leans on.
// TestCursorInputGateUnanchoredWidgetIsTheNamedResidual pins a RESIDUAL, not a
// protection: a decision surface no anchor covers is delivered into, and it always
// was — the old rule only ever refused this pane because a stuck verdict happened
// to fail an unrelated recovery condition, which the old test said in as many
// words. What keeps the residual small is that the anchors ARE exhaustive over the
// surfaces cursor raises on its own; a widget outside that set is one a person
// opened at the keyboard.
func TestCursorInputGateUnanchoredWidgetIsTheNamedResidual(t *testing.T) {
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
	// still drawn beneath it, and a stability verdict of idle — NOT the settled needs_input
	// the recovery requires, so the arm rejects.
	unknownModal := paneFixture(t, "cursor-ready") + "\n Some future approval widget?\n   → Yes, do it (y)\n"
	if approvalAnchorFor(KindCursor).MatchString(unknownModal) {
		t.Fatal("premise: this pane must NOT match the approval anchor")
	}
	if !h.inputAccepted(w, ActivityIdle, KindCursor, unknownModal, unknownModal) {
		t.Error("the residual: an unanchored widget is not detected")
	}
	// And the anchored ones still refuse — that is what keeps the residual narrow.
	dialog := paneFixture(t, "cursor-ready-approval-shell")
	if h.inputAccepted(w, ActivityNeedsInput, KindCursor, dialog, dialog) {
		t.Error("a known dialog on the visible frame must still refuse")
	}

	// The legitimate case is unaffected: when `stop` DOES fire (reliably only on turn 1 on
	// current builds) it settles the fold, and a settled verdict accepts however long it
	// has been quiet.
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
// TestCursorInputGateAcceptsWhileWorking is the assertion the rule change
// INVERTS: a working agent queues the line rather than losing it, so refusing was
// never protecting anything.
func TestCursorInputGateAcceptsWhileWorking(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)

	w := newCursorWatcher("", nil)
	w.pushHookEvent(hookEv("beforeSubmitPrompt", `{"session_id":"`+cursorTestSessionID+`","prompt":"go"}`))
	w.refresh(clk.now())
	if !h.inputAccepted(w, ActivityNeedsInput, KindCursor, paneFixture(t, "cursor-ready"), paneFixture(t, "cursor-ready")) {
		t.Error("a working hook verdict is not a blocked one: the TUI queues the line")
	}

	// …and accepts once `stop` lands (the settled boundary).
	w.pushHookEvent(hookEv("stop", `{"session_id":"`+cursorTestSessionID+`","status":"completed"}`))
	w.refresh(clk.now())
	if !h.inputAccepted(w, ActivityIdle, KindCursor, paneFixture(t, "cursor-ready"), paneFixture(t, "cursor-ready")) {
		t.Error("a settled hook verdict must accept input")
	}
}

// --- plan 008 Finding-B: the expired-working cursor guarded recovery ---
//
// cursor's `stop` hook only fires reliably on a session's FIRST turn on current builds, so
// past turn 1 the fold sticks at expired-working forever. The old C4 arm blanket-rejected
// every /input on such a fold, breaking the phone-steering path after one turn even when the
// pane sits at a clean idle composer. The arm now RECOVERS input when the fresh visible pane
// shows no known approval surface AND pane-stability has settled on needs_input — while still
// making typing-into-a-dialog impossible via the exhaustive ApprovalAnchor.
//
// expiredCursorWatcher is a scripted expired-working cursor verdict: activity=working with
// authority demoted past the grace, the state a stuck fold is in.
func expiredCursorWatcher() *stubWatcher {
	return &stubWatcher{activity: ActivityWorking, expiredWorking: true}
}

// (1) THE F1-SAFETY PIN. An expired-working cursor fold whose FRESH visible pane matches the
// ApprovalAnchor is a real dialog on screen: /input must REJECT even though stability has
// settled on needs_input (cursor draws its composer, disabled, under the dialog, so the
// needs_input verdict is real and would otherwise be the recovery's green light). This is the
// load-bearing test — if it ever accepts, a posted "y" answers an approval nobody meant to
// give.
func TestCursorInputGateExpiredWorkingRealDialogRejected(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)

	approval := paneFixture(t, "cursor-ready-approval-shell")
	if !approvalAnchorFor(KindCursor).MatchString(approval) {
		t.Fatal("premise: the approval fixture must match the ApprovalAnchor on the visible frame")
	}
	// stability=needs_input is the worst case: the composer under the dialog settles, so the
	// recovery's condition (b) holds — only condition (a) (no anchor) stands between a posted
	// line and the widget. The anchor MUST be that guard.
	for _, fx := range []string{"cursor-ready-approval-shell", "cursor-ready-approval-delete", "cursor-ready-approval-write"} {
		dialog := paneFixture(t, fx)
		if h.inputAccepted(expiredCursorWatcher(), ActivityNeedsInput, KindCursor, dialog, dialog) {
			t.Errorf("%s: an expired-working cursor with a real dialog on the visible frame must REJECT", fx)
		}
	}
}

// (2) THE RECOVERY (Finding-B). An expired-working cursor fold whose fresh visible pane is a
// clean idle composer (ApprovalAnchor absent) AND whose stability has settled on needs_input
// ACCEPTS — this is the phone-steering path working past turn 1 without a watcher rebuild.
func TestCursorInputGateExpiredWorkingIdleComposerRecovers(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)

	ready := paneFixture(t, "cursor-ready")
	if approvalAnchorFor(KindCursor).MatchString(ready) {
		t.Fatal("premise: the clean composer must NOT match the ApprovalAnchor")
	}
	if !h.inputAccepted(expiredCursorWatcher(), ActivityNeedsInput, KindCursor, ready, ready) {
		t.Error("expired-working cursor + clean idle composer + settled needs_input must ACCEPT (recovery)")
	}
	// Scrollback is not evidence about the present: a dialog answered long ago sits in the
	// 200-line history (the `pane` arg) while the visible frame is clean — still a recovery.
	stale := paneFixture(t, "cursor-ready-approval-shell")
	if !h.inputAccepted(expiredCursorWatcher(), ActivityNeedsInput, KindCursor, stale, ready) {
		t.Error("a dialog only in scrollback must not block the recovery (the anchor reads the visible frame)")
	}
}

// (3) NO FALSE ACCEPT MID-WORK. Anchor absent but stability NOT settled at needs_input →
// REJECT: a working stability keeps the merge at working (the first arm bites), and even a
// settled-but-idle stability (the pane stopped without reaching the composer verdict) fails
// the recovery's condition (b).
// TestCursorInputGateAcceptsOnAStuckExpiredVerdict: cursor's `stop` hook fires
// reliably only on a session's FIRST turn, so its verdict sits stuck at
// expired-working forever after — which under the old rule meant phone steering
// worked exactly once per session and needed a guarded recovery arm to claw back.
// With working no longer a rejection, the stuck-verdict problem stops mattering:
// there is nothing to recover, because nothing was taken away.
func TestCursorInputGateAcceptsOnAStuckExpiredVerdict(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)

	ready := paneFixture(t, "cursor-ready")
	// Still churning: stability=working ⇒ merged stays working ⇒ reject.
	if !h.inputAccepted(expiredCursorWatcher(), ActivityWorking, KindCursor, ready, ready) {
		t.Error("stuck expired-working + churning stability: no dialog, so deliver")
	}
	// Settled but idle (no composer verdict): passes the merge but fails recovery condition
	// (b), so the guarded arm itself rejects.
	if !h.inputAccepted(expiredCursorWatcher(), ActivityIdle, KindCursor, ready, ready) {
		t.Error("stuck expired-working + settled-idle stability: no dialog, so deliver")
	}
}

// (4) REGRESSION: the fresh-fold first-turn path is untouched. On turn 1 `stop` fires, the
// fold is fresh+settled, and /input accepts outright (the structured signal is authoritative)
// — the phone-steering path a session was always able to use on its first turn.
func TestCursorInputGateFreshFirstTurnAccepts(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)

	w := newCursorWatcher("", nil)
	w.pushHookEvent(hookEv("beforeSubmitPrompt", `{"session_id":"`+cursorTestSessionID+`","prompt":"go"}`))
	w.refresh(clk.now())
	w.pushHookEvent(hookEv("stop", `{"session_id":"`+cursorTestSessionID+`","status":"completed"}`))
	w.refresh(clk.now())
	if _, _, fresh, expired := w.snapshot(clk.now()); !fresh || expired {
		t.Fatalf("premise: a just-settled fold must be fresh, not expired (fresh=%v expired=%v)", fresh, expired)
	}
	ready := paneFixture(t, "cursor-ready")
	if !h.inputAccepted(w, ActivityNeedsInput, KindCursor, ready, ready) {
		t.Error("a fresh settled cursor fold (turn 1) must accept /input")
	}
}

// (5) CODEX UNAFFECTED. The guarded arm is gated on ComposerUnderModal, which is FALSE for
// codex (its overlay replaces the composer), so the arm never runs for codex — its
// expired-working behavior is exactly what it was: a settled needs_input at a clean composer
// accepts via the prompt-anchor path, and a codex dialog on the visible frame rejects via
// codex's own ApprovalAnchor.
func TestCursorInputGateCodexExpiredWorkingUnchanged(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)

	if composerUnderModal(KindCodex) {
		t.Fatal("premise: codex must NOT declare ComposerUnderModal (its overlay hides the composer)")
	}
	ready := codexReadyPane()
	if !h.inputAccepted(expiredCursorWatcher(), ActivityNeedsInput, KindCodex, ready, ready) {
		t.Error("codex expired-working + settled needs_input + clean composer must accept (unchanged)")
	}
	dialog := paneFixture(t, "codex-ready-approval-exec")
	if h.inputAccepted(expiredCursorWatcher(), ActivityNeedsInput, KindCodex, dialog, dialog) {
		t.Error("codex expired-working + a dialog on the visible frame must reject via codex's own anchor")
	}
}
