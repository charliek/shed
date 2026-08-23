package rc

import "testing"

// codexPaneAfterATurn is the REAL pane of a codex session that had run one
// turn, captured from a shed in August 2026: the startup banner long since
// scrolled out of the capture window, the composer showing codex's current
// wording.
const codexPaneAfterATurn = `
  The best contributor overview is docs/development/
  architecture.md.

─ Worked for 1m 02s ────────────────────────────────


› Ask Codex to do anything

  gpt-5.6-sol default · ~/prox
`

// TestCodexAtItsComposerIsReadyWhateverTheWording pins the lived failure: the
// anchor knew only the OLD placeholder, so once the banner scrolled away this
// pane classified starting — a blocking lifecycle, so the hub refused typed
// input and showed no activity for a session plainly waiting for a person.
func TestCodexAtItsComposerIsReadyWhateverTheWording(t *testing.T) {
	if got := classifyCodex(KindCodex, codexPaneAfterATurn).State; got != StateReady {
		t.Fatalf("current codex wording: got %q, want %q", got, StateReady)
	}
	// The older wording keeps working: an anchor is a vendor string, and
	// recognizing a new build must not stop recognizing an old one.
	if got := classifyCodex(KindCodex, "  › Find and fix a bug in @filename").State; got != StateReady {
		t.Fatalf("previous codex wording: got %q, want %q", got, StateReady)
	}
	// The same pane is what the INPUT gate reads, so a session that is ready to
	// be typed at must also present its prompt anchor.
	if !codexPromptAnchorRe.MatchString(codexPaneAfterATurn) {
		t.Fatal("the prompt anchor must match the pane the ready check accepted")
	}
}
