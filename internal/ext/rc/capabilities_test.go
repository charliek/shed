package rc

import "testing"

// kindFeatures() is asserted LIVE here (not only via the shared golden fixture — see
// golden_test.go's doc comment: the fixture is byte-identical across several in-repo
// copies incl. out-of-scope desktop/, so the matrix is proven against the function
// directly as well as pinned on the wire).

// TestKindFeatures pins the NORMATIVE per-kind matrix exhaustively — every field of
// every kind kindFeatures() carries an entry for, plus the deliberate omission of
// claude-broker and shell. codex and opencode light up the message feed + gated input
// (their watchers, watch_codex.go / watch_opencode.go, fold a normalized feed and
// gate input on the composer anchor); claude-rc and cursor carry the activity feed
// only (the stability/transcript engines derive activity for them, but no message
// feed exists). Every kind is a TUI lane: approvals on the pane, attach over tmux, no
// interrupt verb in this phase.
func TestKindFeatures(t *testing.T) {
	kf := kindFeatures()

	cases := []struct {
		kind Kind
		want KindFeatures
	}{
		{KindClaudeRC, KindFeatures{PostInput: true, Approvals: "tui", Watch: false, Input: "", Feed: "activity", Interrupt: false, Attach: "tmux"}},
		{KindCodex, KindFeatures{PostInput: true, Approvals: "tui", Watch: true, Input: "gated", Feed: "messages", Interrupt: false, Attach: "tmux"}},
		{KindOpencode, KindFeatures{PostInput: true, Approvals: "tui", Watch: true, Input: "gated", Feed: "messages", Interrupt: false, Attach: "tmux"}},
		{KindCursor, KindFeatures{PostInput: true, Approvals: "tui", Watch: false, Input: "", Feed: "activity", Interrupt: false, Attach: "tmux"}},
	}
	for _, c := range cases {
		t.Run(string(c.kind), func(t *testing.T) {
			features, ok := kf[c.kind]
			if !ok {
				t.Fatalf("kindFeatures() missing an entry for %q", c.kind)
			}
			if features != c.want {
				t.Errorf("kind_features[%q] = %+v, want %+v", c.kind, features, c.want)
			}
		})
	}

	// claude-broker (driven from claude.ai, not the pane) and shell (no agent
	// approval surface) are OMITTED entirely — an absent entry is the wire's way of
	// saying "no feed/input/approval affordances", and a client must keep reading it
	// that way rather than expecting an all-false row.
	for _, k := range []Kind{KindClaudeBroker, KindShell} {
		t.Run("omitted/"+string(k), func(t *testing.T) {
			if features, ok := kf[k]; ok {
				t.Errorf("kind_features must omit %q entirely, got %+v", k, features)
			}
		})
	}
	if len(kf) != len(cases) {
		t.Errorf("kind_features has %d rows, want exactly the %d pinned above", len(kf), len(cases))
	}

	// opencode must carry the IDENTICAL shape as codex (parity is the point of the
	// feed work) — guard against a future edit that special-cases one kind and drifts
	// the other.
	if oc, codex := kf[KindOpencode], kf[KindCodex]; oc != codex {
		t.Errorf("KindOpencode features %+v != KindCodex features %+v, want parity", oc, codex)
	}
}

// TestKindFeaturesWatchFeedLockstep is the deprecation invariant: `watch` is the
// deprecated spelling of `feed == "messages"`, and the producer must keep the two in
// lockstep for every kind until watch is removed. A v1 client reading watch and a v2
// client reading feed must never disagree about the same session.
func TestKindFeaturesWatchFeedLockstep(t *testing.T) {
	for kind, features := range kindFeatures() {
		t.Run(string(kind), func(t *testing.T) {
			if want := features.Feed == "messages"; features.Watch != want {
				t.Errorf("watch = %v but feed = %q; watch must equal (feed == %q)",
					features.Watch, features.Feed, "messages")
			}
		})
	}
}

// TestLaneForKind: every kind (and every unregistered/unknown kind) derives the TUI
// lane in this phase, and the derivation never returns "" — `lane` is always present
// on the wire.
func TestLaneForKind(t *testing.T) {
	kinds := append(append([]Kind(nil), allKinds...), Kind("some-future-kind"), Kind(""))
	for _, k := range kinds {
		t.Run(string(k), func(t *testing.T) {
			if got := laneForKind(k); got != LaneTUI {
				t.Errorf("laneForKind(%q) = %q, want %q", k, got, LaneTUI)
			}
		})
	}
}
