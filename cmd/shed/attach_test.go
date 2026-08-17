package main

import (
	"strings"
	"testing"

	"github.com/charliek/shed/internal/ext/rc"
)

// TestReportRCCreateOutcome pins the RC-create exit contract: a dead-on-create is a
// session-level failure (handled + non-nil error → non-zero exit), while needs-auth /
// needs-trust leave the session running and exit 0, and a live (ready/starting) session
// is not handled here (the caller attaches or prints its summary).
func TestReportRCCreateOutcome(t *testing.T) {
	tests := []struct {
		name        string
		state       rc.State
		kind        string
		wantHandled bool
		wantErr     bool
		wantMsg     string // substring the printed guidance must contain
	}{
		{
			name:        "dead exits non-zero",
			state:       rc.StateDead,
			kind:        "codex",
			wantHandled: true,
			wantErr:     true,
			wantMsg:     "died immediately",
		},
		{
			name:        "needs-auth handled, exits zero",
			state:       rc.StateNeedsAuth,
			kind:        "codex",
			wantHandled: true,
			wantErr:     false,
			wantMsg:     "not logged in",
		},
		{
			// The registry-sourced tool name (rc.ToolFor) replaces the old
			// hand-hardcoded "Claude"/"the agent" — an opencode needs-auth message
			// must name opencode, not "the agent".
			name:        "needs-auth names the actual tool, not a hardcoded agent",
			state:       rc.StateNeedsAuth,
			kind:        "opencode",
			wantHandled: true,
			wantErr:     false,
			wantMsg:     "opencode is not logged in",
		},
		{
			name:        "needs-trust handled, exits zero",
			state:       rc.StateNeedsTrust,
			kind:        "codex",
			wantHandled: true,
			wantErr:     false,
			wantMsg:     "trust prompt",
		},
		{
			name:        "ready is not handled here",
			state:       rc.StateReady,
			kind:        "codex",
			wantHandled: false,
			wantErr:     false,
		},
		{
			name:        "starting is not handled here",
			state:       rc.StateStarting,
			kind:        "codex",
			wantHandled: false,
			wantErr:     false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			dto := rc.Session{Slug: "abc234", State: tc.state, Kind: rc.Kind(tc.kind)}
			handled, err := reportRCCreateOutcome(&b, "myshed", "abc234", tc.kind, dto)
			if handled != tc.wantHandled {
				t.Errorf("handled = %v, want %v", handled, tc.wantHandled)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantMsg != "" && !strings.Contains(b.String(), tc.wantMsg) {
				t.Errorf("output %q does not contain %q", b.String(), tc.wantMsg)
			}
		})
	}
}

// TestRCCreateRequestedWorkdirAlone pins the --workdir fix: --workdir alone (no
// --kind/--prompt/other RC flag) must still route `shed attach` to RC create rather than
// silently falling through to plain tmux attach and discarding the workdir.
func TestRCCreateRequestedWorkdirAlone(t *testing.T) {
	origWorkdir := attachWorkdirFlag
	t.Cleanup(func() { attachWorkdirFlag = origWorkdir })

	attachWorkdirFlag = ""
	if rcCreateRequested() {
		t.Fatal("no RC flags set: rcCreateRequested should be false")
	}

	attachWorkdirFlag = "/home/shed/proj"
	if !rcCreateRequested() {
		t.Fatal("--workdir alone must request RC create, not fall through to plain attach")
	}
}

// TestResolveRCPermMode pins the shared --skip/--permission-mode handling used by
// both `shed attach` and `shed plan`: mutual exclusion, per-kind validity
// (rc.PermModeAcceptedBy), and the default fallback when neither flag is given.
func TestResolveRCPermMode(t *testing.T) {
	t.Run("skip and permission-mode are mutually exclusive", func(t *testing.T) {
		if _, err := resolveRCPermMode("claude-rc", "auto", true, ""); err == nil {
			t.Fatal("want error for --skip + --permission-mode together")
		}
	})
	t.Run("skip shorthand maps to generic skip", func(t *testing.T) {
		mode, err := resolveRCPermMode("codex", "", true, "")
		if err != nil || mode != rc.PermModeSkip {
			t.Fatalf("got (%q, %v), want (%q, nil)", mode, err, rc.PermModeSkip)
		}
	})
	t.Run("neither flag falls back to dflt", func(t *testing.T) {
		mode, err := resolveRCPermMode("codex", "", false, rc.PermModeAuto)
		if err != nil || mode != rc.PermModeAuto {
			t.Fatalf("got (%q, %v), want (%q, nil)", mode, err, rc.PermModeAuto)
		}
	})
	t.Run("empty dflt means no posture flag", func(t *testing.T) {
		mode, err := resolveRCPermMode("codex", "", false, "")
		if err != nil || mode != "" {
			t.Fatalf("got (%q, %v), want (\"\", nil)", mode, err)
		}
	})
	t.Run("explicit mode passes through when valid for the kind", func(t *testing.T) {
		mode, err := resolveRCPermMode("codex", "auto", false, "")
		if err != nil || mode != "auto" {
			t.Fatalf("got (%q, %v), want (\"auto\", nil)", mode, err)
		}
	})
	t.Run("claude-only mode on a non-claude kind is rejected (like attach)", func(t *testing.T) {
		if _, err := resolveRCPermMode("codex", "acceptEdits", false, ""); err == nil {
			t.Fatal("want error: acceptEdits is claude-only")
		}
	})
	t.Run("claude-only mode accepted on a claude kind", func(t *testing.T) {
		mode, err := resolveRCPermMode("claude-rc", "acceptEdits", false, "")
		if err != nil || mode != "acceptEdits" {
			t.Fatalf("got (%q, %v), want (\"acceptEdits\", nil)", mode, err)
		}
	})
}
