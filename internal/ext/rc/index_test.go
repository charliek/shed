package rc

import "testing"

func TestIndexByTmux(t *testing.T) {
	sessions := []Session{
		{Slug: "abc234", TmuxSession: "rc-abc234", Kind: KindClaudeRC},
		{Slug: "def567", TmuxSession: "rc-def567", Kind: KindShell},
		{Slug: "noname"}, // no tmux_session -> skipped (can't be merged onto a row)
	}
	m := IndexByTmux(sessions)
	if len(m) != 2 {
		t.Fatalf("want 2 keyed entries (empty tmux skipped), got %d: %v", len(m), m)
	}
	if s, ok := m["rc-abc234"]; !ok || s.Kind != KindClaudeRC {
		t.Errorf("rc-abc234 lookup wrong: %+v ok=%v", s, ok)
	}
	if _, ok := m["rc-def567"]; !ok {
		t.Error("rc-def567 missing")
	}
	if _, ok := m[""]; ok {
		t.Error("empty tmux_session must not be keyed")
	}
}
