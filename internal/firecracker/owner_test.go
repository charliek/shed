//go:build linux
// +build linux

package firecracker

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// TestRequiredOwnerIsCheckedAgainstTheLoadedShed: with a required owner in
// the context (a service client's request on a shed it owned when the API
// server looked), every operation checks the owner of the shed it actually
// loads, under the per-shed lock. A shed that was deleted and recreated
// under the name by an admin is not found, and is left untouched.
func TestRequiredOwnerIsCheckedAgainstTheLoadedShed(t *testing.T) {
	c, cfg := newP9TeardownClient(t)
	writeResumeInstance(t, cfg.InstanceDir, "nocturn-a", config.StatusStopped, 0) // admin's: no owner
	ctx := backend.WithRequiredOwner(context.Background(), "nocturn")
	notFound := func(op string, err error) {
		t.Helper()
		if !errors.Is(err, config.ErrShedNotFoundSentinel) {
			t.Errorf("%s on an admin shed with a required owner = %v, want not found", op, err)
		}
	}

	_, err := c.GetShed(ctx, "nocturn-a")
	notFound("GetShed", err)
	_, err = c.StartShed(ctx, "nocturn-a")
	notFound("StartShed", err)
	_, err = c.StopShed(ctx, "nocturn-a")
	notFound("StopShed", err)
	notFound("DeleteShed", c.DeleteShed(ctx, "nocturn-a"))
	notFound("Exec", NewBackend(c).Exec(ctx, "nocturn-a", backend.ExecOptions{Cmd: []string{"true"}}))

	meta, err := LoadMetadata(cfg.InstanceDir, "nocturn-a")
	if err != nil {
		t.Fatalf("the admin's shed was removed: %v", err)
	}
	if meta.Status != config.StatusStopped {
		t.Errorf("the admin's shed status = %q, want stopped", meta.Status)
	}

	// The client's own shed passes the check.
	meta.Owner = "nocturn"
	if err := meta.Save(cfg.InstanceDir); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetShed(ctx, "nocturn-a"); err != nil {
		t.Errorf("GetShed on the client's own shed = %v", err)
	}
	if err := c.DeleteShed(ctx, "nocturn-a"); err != nil {
		t.Errorf("DeleteShed on the client's own shed = %v", err)
	}
}

// TestDeleteStoppedShedStopsItsMessageChannel: a Stopped shed can still hold
// a message channel (a VMM exit that GetShed reconciled, a failed create's
// leftovers). Delete stops it, so a shed created under the name later never
// finds it.
func TestDeleteStoppedShedStopsItsMessageChannel(t *testing.T) {
	cfg := testFirecrackerConfig(t.TempDir())
	cfg.UppersDir = filepath.Join(cfg.InstanceDir, "uppers")
	c, bridge := newResumeTestClient(t, cfg, nil)
	nm, err := NewNetworkManager(cfg.BridgeName, cfg.BridgeCIDR, cfg.TAPPrefix)
	if err != nil {
		t.Fatalf("NewNetworkManager: %v", err)
	}
	c.netMgr = nm
	writeResumeInstance(t, cfg.InstanceDir, "alpha", config.StatusStopped, 0)
	c.credMgr.ResumeMessageChannel("alpha", c.newAgentClient("alpha"), false)
	if got := resumedNames(bridge); len(got) != 1 {
		t.Fatalf("fixture: bridge sheds = %v, want [alpha]", got)
	}

	if err := c.DeleteShed(context.Background(), "alpha"); err != nil {
		t.Fatalf("DeleteShed: %v", err)
	}
	if got := resumedNames(bridge); len(got) != 0 {
		t.Errorf("the deleted shed's channel is still registered on the bridge: %v", got)
	}
}
