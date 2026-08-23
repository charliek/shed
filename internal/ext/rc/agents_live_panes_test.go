package rc

import "testing"

// Panes captured from LIVE sessions on 2026-08-23, in the states a long-running
// agent actually passes through. Every one of them classified `starting` before
// the anchors below were widened — and `starting` is a BLOCKING lifecycle, so
// each was a session that could not be typed at, watched, or steered while
// being plainly alive on screen.
//
// The lesson they encode: anchor on the TUI's own CHROME (an interrupt hint, a
// dialog frame), not on placeholder prose. Placeholder text is the part vendors
// reword between releases, and the part that disappears mid-session.
const (
	// codex, after a turn: banner long scrolled away, current composer wording.
	codexLivePaneIdle = `
─ Worked for 1m 02s ────────────────────────────────

› Ask Codex to do anything

  gpt-5.6-sol default · ~/prox
`
	// codex, mid-turn. Note the composer line is STILL DRAWN while it works —
	// which is why the composer cannot be read as "waiting for input".
	codexLivePaneWorking = `
• Working (2s • esc to interrupt)

› Ask Codex to do anything

  gpt-5.6-sol default · ~/prox
`
	// cursor, mid-turn: the composer line carries a RIGHT-ALIGNED hint, so a
	// bare-line anchor no longer matches it.
	cursorLivePaneWorking = `
  → Add a follow-up                                             ctrl+c to stop


  Cursor Grok 4.6 High Fast · 13.6%
  ~/prox · main
`
	// cursor, after somebody TYPED into it: the placeholder is replaced by their
	// words, so nothing about the composer's CONTENT can be anchored on.
	cursorLivePaneTyped = `
  → queued cursor follow-up


  Cursor Grok 4.6 High Fast · 14.2%
  ~/prox · main
`
	// cursor, settled after a turn.
	cursorLivePaneIdle = `
  → Add a follow-up


  Cursor Grok 4.6 High Fast · 14.1%
  ~/prox · main
`
	// cursor, blocked on a command approval: the modal covers the composer
	// entirely, so the ready anchor cannot see it.
	cursorLivePaneApproval = `
 $  ls -1 /home/shed/prox && echo "---" && git -C /home/shed/prox log -5

 Run this command?
 Not in allowlist: echo, git -C
  → Run (once) (y)
    Add Shell(echo), Shell(git -C) to allowlist? (tab)
    Run Everything (shift+tab)
    Skip & tell the agent what to do instead (esc or n)
`
)

// TestLivePanesClassifyAsUp asserts every captured state is READY: the
// lifecycle question is "is this agent up", not "is it idle" — what it is doing
// is the activity dimension's job, derived separately.
func TestLivePanesClassifyAsUp(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    Kind
		pane    string
		classfn func(Kind, string) PaneResult
	}{
		{"codex idle", KindCodex, codexLivePaneIdle, classifyCodex},
		{"codex working", KindCodex, codexLivePaneWorking, classifyCodex},
		{"cursor working", KindCursor, cursorLivePaneWorking, classifyCursor},
		{"cursor idle", KindCursor, cursorLivePaneIdle, classifyCursor},
		{"cursor with typed text", KindCursor, cursorLivePaneTyped, classifyCursor},
		{"cursor at an approval", KindCursor, cursorLivePaneApproval, classifyCursor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.classfn(tc.kind, tc.pane).State; got != StateReady {
				t.Fatalf("state = %q, want %q", got, StateReady)
			}
		})
	}
}

// TestLiveApprovalPaneStillAnchors guards the pairing the input gate depends on:
// the approval pane now classifies READY, so the only thing standing between a
// remote keystroke and that dialog is the approval anchor. It must match.
func TestLiveApprovalPaneStillAnchors(t *testing.T) {
	anchor := approvalAnchorFor(KindCursor)
	if anchor == nil {
		t.Fatal("cursor must declare an approval anchor")
	}
	if !anchor.MatchString(cursorLivePaneApproval) {
		t.Fatal("the approval anchor must match the live approval pane")
	}
	for name, pane := range map[string]string{
		"working": cursorLivePaneWorking,
		"idle":    cursorLivePaneIdle,
	} {
		if anchor.MatchString(pane) {
			t.Errorf("%s pane must NOT look like an approval", name)
		}
	}
}
