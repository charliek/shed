package rc

import (
	"slices"
	"testing"
)

// kindFeatures() is asserted LIVE here (not only via the shared golden fixture — see
// golden_test.go's doc comment: the fixture is byte-identical across several in-repo
// copies incl. out-of-scope desktop/, so the matrix is proven against the function
// directly as well as pinned on the wire).

// TestKindFeatures pins the NORMATIVE per-kind matrix exhaustively — every field of
// every kind kindFeatures() carries an entry for, plus the deliberate omission of
// claude-broker and shell. codex, opencode and cursor light up the message feed (their
// watchers — watch_codex.go / watch_opencode.go / watch_cursor.go — fold a normalized
// feed, the last of them from hook events pushed into the hub); claude-rc carries the
// activity feed only (the transcript engine derives activity for it, but no message feed
// exists). OPENCODE is the live lane: its embedded HTTP+SSE server takes whole turns,
// interrupts and approvals through the hub, so its row alone reads
// input "turn" / approvals "remote" / interrupt true. Every other kind
// is TUI-lane: approvals on the pane, no interrupt verb, gated line input at most.
func TestKindFeatures(t *testing.T) {
	kf := kindFeatures()

	cases := []struct {
		kind Kind
		want KindFeatures
	}{
		{KindClaudeRC, KindFeatures{PostInput: true, Approvals: "tui", Watch: false, Input: "", Feed: "activity", Interrupt: false, Attach: "tmux"}},
		{KindCodex, KindFeatures{PostInput: true, Approvals: "tui", Watch: true, Input: "gated", Feed: "messages", Interrupt: false, Attach: "tmux"}},
		{KindOpencode, KindFeatures{PostInput: true, Approvals: "remote", Watch: true, Input: "turn", Feed: "messages", Interrupt: true, Attach: "tmux"}},
		{KindCursor, KindFeatures{PostInput: true, Approvals: "tui", Watch: true, Input: "gated", Feed: "messages", Interrupt: false, Attach: "tmux"}},
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

	// There was a codex==opencode PARITY assertion here through plan 007: while both
	// kinds were feed+gated-input TUI sessions, drift between them was a bug. The
	// divergence is now INTENTIONAL — opencode's embedded server makes it the first
	// live lane (turn/interrupt/remote approvals) and codex has no equivalent surface —
	// so the parity guard is deliberately gone. The exhaustive rows above are the
	// guard: either kind's row moving is a visible edit here.
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

// TestCapabilityFeatures pins the advertised feature set BY VALUE, in order. The
// tokens are a public contract (clients gate behavior on them), so adding, renaming or
// reordering one is a wire change that must be a deliberate edit here — not a silent
// side effect. `contract-v2` in particular must ship in the same change as the routes
// it advertises: a token that arrives early tells clients to call verbs that 404.
func TestCapabilityFeatures(t *testing.T) {
	want := []string{"generic-perm", "plan-stdin", "prompt-b64", "serve", "activity", "messages", "contract-v2"}
	if !slices.Equal(capabilityFeatures, want) {
		t.Errorf("capabilityFeatures = %v, want %v", capabilityFeatures, want)
	}

	// The assembled payload carries the same list — and a COPY of it, so a client-side
	// mutation of the returned slice can never rewrite the package's own token list.
	caps := BuildCapabilities(func(string) AgentInfo { return AgentInfo{} }, nil)
	if caps.RCVersion != CapabilityVersion {
		t.Errorf("rc_version = %d, want %d", caps.RCVersion, CapabilityVersion)
	}
	if !slices.Equal(caps.Features, want) {
		t.Errorf("BuildCapabilities features = %v, want %v", caps.Features, want)
	}
	if len(caps.Features) > 0 && &caps.Features[0] == &capabilityFeatures[0] {
		t.Error("BuildCapabilities must hand out a copy of capabilityFeatures, not the slice itself")
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
