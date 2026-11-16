package sshd

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// TestForbiddenMessageNamesTheRefusal: an admin refused entry to a shed with
// no user is told how it gets one, not that it belongs to someone else.
func TestForbiddenMessageNamesTheRefusal(t *testing.T) {
	for _, tc := range []struct {
		shed config.Shed
		want string
	}{
		{config.Shed{Name: "legacy", Mounts: config.SelectedMounts(nil)}, "shed 'legacy' has no user; nobody can enter it until an admin gives it to a user with 'shed assign legacy <user>'"},
		{config.Shed{Name: "box", User: "alice", Mounts: config.SelectedMounts(nil)}, "shed 'box' belongs to another user"},
	} {
		err := backend.AuthorizeShed(backend.UserPrincipal("root", true), &tc.shed, backend.ActionEnter)
		if !errors.Is(err, config.ErrForbiddenSentinel) {
			t.Fatalf("%s: %v, want forbidden", tc.shed.Name, err)
		}
		if got := forbiddenMessage(tc.shed.Name, fmt.Errorf("wrapped: %w", err)); !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s: message %q, want %q", tc.shed.Name, got, tc.want)
		}
	}
}
