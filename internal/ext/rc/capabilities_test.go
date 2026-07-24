package rc

import "testing"

// kindFeatures() is asserted LIVE here (not via the shared golden fixture — see
// golden_test.go's doc comment: the fixture is byte-identical across 3 in-repo copies
// incl. out-of-scope desktop/, so a new watchable kind is proven against the function
// directly rather than by editing that shared contract).

// TestKindFeatures asserts the per-kind feature shape kindFeatures() advertises for
// every kind it carries an entry for: opencode now lights up Watch/Input at parity
// with codex — opencode produces a message feed + gated input via its HTTP/SSE
// watcher (watch_opencode.go/watch_opencode_transport.go) the same way codex does via
// its rollout JSONL watcher (watch_codex.go) — while every other kind stays
// watch/gated-input-free (the feed stays codex+opencode-only this phase).
func TestKindFeatures(t *testing.T) {
	kf := kindFeatures()

	cases := []struct {
		kind          Kind
		wantWatch     bool
		wantInput     string
		wantPostInput bool
		wantApprovals string
	}{
		{KindClaudeRC, false, "", true, "tui"},
		{KindCodex, true, "gated", true, "tui"},
		{KindOpencode, true, "gated", true, "tui"},
		{KindCursor, false, "", true, "tui"},
	}
	for _, c := range cases {
		t.Run(string(c.kind), func(t *testing.T) {
			features, ok := kf[c.kind]
			if !ok {
				t.Fatalf("kindFeatures() missing an entry for %q", c.kind)
			}
			if features.Watch != c.wantWatch {
				t.Errorf("Watch = %v, want %v", features.Watch, c.wantWatch)
			}
			if features.Input != c.wantInput {
				t.Errorf("Input = %q, want %q", features.Input, c.wantInput)
			}
			if features.PostInput != c.wantPostInput {
				t.Errorf("PostInput = %v, want %v (AcceptsTypedInput(%s))", features.PostInput, c.wantPostInput, c.kind)
			}
			if features.Approvals != c.wantApprovals {
				t.Errorf("Approvals = %q, want %q", features.Approvals, c.wantApprovals)
			}
		})
	}

	// opencode must carry the IDENTICAL shape as codex (parity is the point of this
	// change) — guard against a future edit that special-cases one kind and drifts the
	// other.
	if oc, codex := kf[KindOpencode], kf[KindCodex]; oc != codex {
		t.Errorf("KindOpencode features %+v != KindCodex features %+v, want parity", oc, codex)
	}
}
