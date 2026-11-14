package gce

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/remote/provider"
)

// TestLiveReadOnly runs read-only calls against the real Compute API, to
// confirm request and reply shapes. It runs only with SHED_GCE_LIVE=1 and
// the variables the dev environment file defines (SHED_REMOTE_ENV,
// SHED_GCE_PROJECT, SHED_GCE_ZONE, SHED_GCE_NETWORK, SHED_GCE_SUBNET and
// SHED_GCE_CREDENTIALS_FILE or GOOGLE_CREDENTIALS_BASE64). It creates
// nothing.
//
//	set -a; . ~/.config/shed/remote-dev/env; set +a
//	SHED_GCE_LIVE=1 go test ./internal/remote/provider/gce -run TestLive -v
func TestLiveReadOnly(t *testing.T) {
	if os.Getenv("SHED_GCE_LIVE") != "1" {
		t.Skip("set SHED_GCE_LIVE=1 and the dev environment to run")
	}
	creds, err := LoadCredentials(os.Getenv, "GOOGLE_CREDENTIALS_BASE64", "SHED_GCE_CREDENTIALS_FILE")
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{
		Project: os.Getenv("SHED_GCE_PROJECT"),
		Zone:    os.Getenv("SHED_GCE_ZONE"),
		Env:     os.Getenv("SHED_REMOTE_ENV"),
		Network: os.Getenv("SHED_GCE_NETWORK"),
		Subnet:  os.Getenv("SHED_GCE_SUBNET"),
		Token:   creds,
		Backoff: []time.Duration{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	t.Run("list instances by env", func(t *testing.T) {
		insts, err := c.List(ctx, provider.Selector{Env: c.Env()})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%d instances in env", len(insts))
	})
	t.Run("list instances all", func(t *testing.T) {
		insts, err := c.List(ctx, provider.Selector{})
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range insts {
			t.Logf("instance %s %s labels=%v", i.Name, i.State, i.Labels)
		}
	})
	t.Run("list disks", func(t *testing.T) {
		disks, err := c.ListDisks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%d disks in env", len(disks))
	})
	t.Run("network", func(t *testing.T) {
		n, err := c.GetNetwork(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n.AutoCreateSubnetworks {
			t.Errorf("network %s is auto-mode", n.Name)
		}
		t.Logf("network %s subnets=%v policy=%q", n.Name, n.Subnetworks, n.FirewallPolicy)
	})
	t.Run("subnetwork", func(t *testing.T) {
		s, err := c.GetSubnetwork(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("subnet %s region=%s network=%s stack=%s", s.Name, s.Region, s.Network, s.StackType)
	})
	t.Run("firewalls", func(t *testing.T) {
		rules, err := c.ListFirewalls(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rules {
			t.Logf("rule %s %s prio=%d targets=%v allowed=%v", r.Name, r.Direction, r.Priority, r.TargetTags, r.Allowed)
		}
	})
	t.Run("image family", func(t *testing.T) {
		name, err := c.ResolveImage(ctx, "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(name, "projects/ubuntu-os-cloud/global/images/ubuntu-") {
			t.Errorf("resolved %q", name)
		}
		again, err := c.ResolveImage(ctx, name)
		if err != nil || again != name {
			t.Errorf("exact lookup of %q = %q, %v", name, again, err)
		}
		t.Logf("family resolves to %s", name)
	})
	t.Run("missing image", func(t *testing.T) {
		_, err := c.ResolveImage(ctx, "family/shed-no-such-family")
		if !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("got %v, want ErrNotFound", err)
		}
	})
	t.Run("missing instance", func(t *testing.T) {
		_, err := c.Get(ctx, makeRef(c.Zone(), "shed-no-such-instance"))
		if !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("got %v, want ErrNotFound", err)
		}
		p, err := c.Destroy(ctx, makeRef(c.Zone(), "shed-no-such-instance"))
		if err != nil || p.Want != provider.StateAbsent {
			t.Errorf("destroy of a missing instance = %+v, %v", p, err)
		}
		if _, err := c.GuestHostKey(ctx, makeRef(c.Zone(), "shed-no-such-instance")); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("GuestHostKey of a missing instance = %v, want ErrNotFound", err)
		}
	})
	t.Run("missing operation", func(t *testing.T) {
		// 404, not 403, shows the role may wait on operations.
		if _, err := c.waitOperation(ctx, "wait", c.Zone(), "operation-shed-no-such-op"); !errors.Is(err, errOpUnknown) {
			t.Errorf("waitOperation of a missing operation = %v, want errOpUnknown", err)
		}
	})
}
