package remote

import (
	"errors"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider"
)

// TestLimitErrorText pins the refusal a client reads at a reached limit. A
// live run showed "server limit reached: server limit reached: ..." and "1 of
// them are VMs": both sentinels carry the same text and were both printed.
func TestLimitErrorText(t *testing.T) {
	tests := []struct {
		name  string
		what  string
		limit int
		stuck int
		want  string
	}{
		{"no stuck delete", "remote.limits.max_sheds", 8, 0,
			"server limit reached: remote.limits.max_sheds (8) reached"},
		{"one stuck delete", "remote.limits.max_running", 2, 1,
			"server limit reached: remote.limits.max_running (2) reached, counting 1 deleted shed whose VM could not be destroyed yet (see the server log)"},
		{"two stuck deletes", "remote.limits.max_running", 2, 2,
			"server limit reached: remote.limits.max_running (2) reached, counting 2 deleted sheds whose VMs could not be destroyed yet (see the server log)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := limitError(tt.what, tt.limit, tt.stuck)
			if got := err.Error(); got != tt.want {
				t.Errorf("message = %q, want %q", got, tt.want)
			}
			if n := strings.Count(err.Error(), "server limit reached"); n != 1 {
				t.Errorf("the message says \"server limit reached\" %d times, want once", n)
			}
			if !errors.Is(err, config.ErrServerLimitReachedSentinel) {
				t.Error("the error does not match the API's SERVER_LIMIT_REACHED sentinel")
			}
			if !errors.Is(err, provider.ErrLimit) {
				t.Error("the error does not match provider.ErrLimit")
			}
		})
	}
}
