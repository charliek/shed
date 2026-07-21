package rc

import "testing"

// kindFeatures() is asserted LIVE here (not via the shared golden fixture — see
// golden_test.go's doc comment: the fixture is byte-identical across 3 in-repo copies
// incl. out-of-scope desktop/, so a new watchable kind is proven against the function
// directly rather than by editing that shared contract).

// TestKindFeaturesOpencodeMatchesCodex asserts opencode now advertises the same
// watch/gated-input shape as codex: kindFeatures() lights up Watch/Input for both, in
// parallel — opencode produces a message feed + gated input via its HTTP/SSE watcher
// (watch_opencode.go/watch_opencode_transport.go) the same way codex does via its
// rollout JSONL watcher (watch_codex.go).
func TestKindFeaturesOpencodeMatchesCodex(t *testing.T) {
	kf := kindFeatures()

	oc, ok := kf[KindOpencode]
	if !ok {
		t.Fatal("kindFeatures() missing an entry for KindOpencode")
	}
	if !oc.Watch {
		t.Error("KindOpencode.Watch = false, want true")
	}
	if oc.Input != "gated" {
		t.Errorf("KindOpencode.Input = %q, want \"gated\"", oc.Input)
	}
	if !oc.PostInput {
		t.Error("KindOpencode.PostInput = false, want true (AcceptsTypedInput(KindOpencode))")
	}
	if oc.Approvals != "tui" {
		t.Errorf("KindOpencode.Approvals = %q, want \"tui\"", oc.Approvals)
	}

	// Codex must carry the identical shape (parity is the point of this change) — guard
	// against a future edit that special-cases one kind and drifts the other.
	codex, ok := kf[KindCodex]
	if !ok {
		t.Fatal("kindFeatures() missing an entry for KindCodex")
	}
	if oc != codex {
		t.Errorf("KindOpencode features %+v != KindCodex features %+v, want parity", oc, codex)
	}
}

// TestKindFeaturesWatchInputOnlyCodexAndOpencode asserts every OTHER kind still omits
// watch/gated-input — the feed stays codex+opencode-only this phase; nothing else
// should light up incidentally.
func TestKindFeaturesWatchInputOnlyCodexAndOpencode(t *testing.T) {
	kf := kindFeatures()
	for k, features := range kf {
		wantWatch := k == KindCodex || k == KindOpencode
		if features.Watch != wantWatch {
			t.Errorf("kindFeatures()[%q].Watch = %v, want %v", k, features.Watch, wantWatch)
		}
		wantInput := ""
		if wantWatch {
			wantInput = "gated"
		}
		if features.Input != wantInput {
			t.Errorf("kindFeatures()[%q].Input = %q, want %q", k, features.Input, wantInput)
		}
	}
}
