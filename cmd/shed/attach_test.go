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
