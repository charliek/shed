package gce

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/remote/provider"
)

func testEnvRule(sources ...string) provider.FirewallRule {
	return provider.FirewallRule{
		Name: provider.FirewallRuleName(testEnv), Direction: provider.DirectionIngress, Priority: 1000,
		SourceRanges: sources, TargetTags: []string{provider.NetworkTag(testEnv)},
		Allowed: []provider.FirewallPorts{{Protocol: "tcp", Ports: []string{"22"}}},
	}
}

// TestFirewallReads covers what the conformance suite cannot see: the reads
// follow every page, need nothing but the list call, and a rule the API
// returns with no direction is an ingress rule.
func TestFirewallReads(t *testing.T) {
	f := newFakeCompute(t)
	c := testClient(t, f)
	ctx := context.Background()
	f.setFirewall([]provider.FirewallRule{
		{Name: "a-other", Network: "default", Direction: provider.DirectionIngress, SourceRanges: []string{"0.0.0.0/0"},
			Allowed: []provider.FirewallPorts{{Protocol: "tcp", Ports: []string{"22"}}}},
		{Name: "b-stray", SourceRanges: []string{"10.0.0.0/8"}, Allowed: []provider.FirewallPorts{{Protocol: "all"}}},
		testEnvRule("192.0.2.10/32"),
	})
	f.pageSize = 1

	rule, err := c.EnvFirewallRule(ctx)
	if err != nil || rule.Name != "shed-dev-test-ssh" || rule.Network != "shed-net" {
		t.Fatalf("EnvFirewallRule = %+v, %v", rule, err)
	}
	view, err := c.NetworkIngress(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if view.Network != "shed-net" || view.Default || len(view.Rules) != 2 || view.Rules[0].Name != "b-stray" || view.Rules[0].Direction != provider.DirectionIngress {
		t.Errorf("NetworkIngress = %+v", view)
	}
	for _, r := range f.requestsFor("GET", `firewalls`) {
		if !strings.HasSuffix(r.Path, "/global/firewalls") {
			t.Errorf("a firewall read used %s; the base role only allows the list", r.Path)
		}
		if r.Query.Get("filter") != "" {
			t.Errorf("a firewall list sent a filter: %q", r.Query.Get("filter"))
		}
	}
	if n := len(f.requestsFor("GET", `/global/firewalls$`)); n != 6 {
		t.Errorf("%d list requests, want 6 (three pages for each read)", n)
	}

	t.Run("the default network is reported", func(t *testing.T) {
		d := testClient(t, f, func(cfg *Config) { cfg.Network = DefaultNetworkName })
		view, err := d.NetworkIngress(ctx)
		if err != nil || !view.Default || view.Network != "default" || len(view.Rules) != 1 {
			t.Errorf("NetworkIngress on the default network = %+v, %v", view, err)
		}
	})

	t.Run("a failed page is an error, never a short list", func(t *testing.T) {
		f.failNext("GET", `/global/firewalls$`, 403, "forbidden", "Required 'compute.firewalls.list' permission", 1)
		if _, err := c.NetworkIngress(ctx); err == nil {
			t.Error("NetworkIngress returned no error")
		}
		f.failNext("GET", `/global/firewalls$`, 403, "forbidden", "Required 'compute.firewalls.list' permission", 1)
		if _, err := c.EnvFirewallRule(ctx); err == nil || errors.Is(err, provider.ErrNotFound) {
			t.Errorf("EnvFirewallRule = %v, want an error that is not ErrNotFound", err)
		}
	})
}

// TestSetEnvFirewallSources pins the one mutating firewall call: which URL,
// which body, and that its global operation is waited for.
func TestSetEnvFirewallSources(t *testing.T) {
	ctx := context.Background()
	newRules := func(f *fakeCompute) {
		f.setFirewall([]provider.FirewallRule{
			testEnvRule("192.0.2.10/32"),
			{Name: "shed-other-ssh", SourceRanges: []string{"198.51.100.7/32"}, TargetTags: []string{"shed-other"},
				Allowed: []provider.FirewallPorts{{Protocol: "tcp", Ports: []string{"22"}}}},
		})
	}

	t.Run("golden patch", func(t *testing.T) {
		f := newFakeCompute(t)
		newRules(f)
		f.opDelay = 2
		c := testClient(t, f)
		if err := c.SetEnvFirewallSources(ctx, []string{"203.0.113.9/32"}); err != nil {
			t.Fatal(err)
		}
		mutating := append(f.requestsFor("PATCH", `.`), f.requestsFor("PUT", `.`)...)
		mutating = append(mutating, f.requestsFor("DELETE", `.`)...)
		if len(mutating) != 1 {
			t.Fatalf("%d mutating requests, want exactly one PATCH", len(mutating))
		}
		req := mutating[0]
		if want := "projects/test-proj/global/firewalls/shed-dev-test-ssh"; req.Path != want {
			t.Errorf("patched %s, want %s", req.Path, want)
		}
		if rid := req.Query["requestId"]; len(rid) != 1 || len(rid[0]) != 36 {
			t.Errorf("requestId = %v, want one UUID", rid)
		}
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, []byte(req.Body), "", "  "); err != nil {
			t.Fatal(err)
		}
		pretty.WriteByte('\n')
		golden := filepath.Join("testdata", "patch_firewall_sources.json")
		if *update {
			if err := os.WriteFile(golden, pretty.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("%v (run with -update to create it)", err)
		}
		if !bytes.Equal(pretty.Bytes(), want) {
			t.Errorf("patch body differs from %s:\n%s", golden, pretty.String())
		}
		// The operation was waited for at the global URL, never a zone's.
		if n := len(f.requestsFor("POST", `/global/operations/[^/]+/wait$`)); n != 3 {
			t.Errorf("%d global operation waits, want 3", n)
		}
		if n := len(f.requestsFor("POST", `/zones/[^/]+/operations/`)); n != 0 {
			t.Errorf("%d zone operation waits for a global operation", n)
		}
		rule, err := c.EnvFirewallRule(ctx)
		if err != nil || !slices.Equal(rule.SourceRanges, []string{"203.0.113.9/32"}) {
			t.Errorf("rule after the patch = %+v, %v", rule, err)
		}
		view, err := c.NetworkIngress(ctx)
		if err != nil || len(view.Rules) != 2 || !slices.Equal(view.Rules[0].SourceRanges, []string{"203.0.113.9/32"}) ||
			!slices.Equal(view.Rules[1].SourceRanges, []string{"198.51.100.7/32"}) {
			t.Errorf("rules after the patch = %+v, %v", view, err)
		}
	})

	t.Run("invalid ranges make no request", func(t *testing.T) {
		f := newFakeCompute(t)
		newRules(f)
		c := testClient(t, f)
		for _, bad := range [][]string{nil, {"0.0.0.0/0"}, {"203.0.113.9"}, {"203.0.113.9/24"}, {"2001:db8::1/128"}, {"198.51.0.0/16"}} {
			if err := c.SetEnvFirewallSources(ctx, bad); !errors.Is(err, provider.ErrInvalid) {
				t.Errorf("SetEnvFirewallSources(%v) = %v, want ErrInvalid", bad, err)
			}
		}
		if n := len(f.requestsFor("PATCH", `.`)); n != 0 {
			t.Errorf("%d PATCH requests", n)
		}
	})

	tests := []struct {
		name    string
		arrange func(f *fakeCompute)
		wantIs  error
		wantMsg string
	}{
		{"missing rule", func(f *fakeCompute) { f.setFirewall(nil) }, provider.ErrNotFound, ""},
		{"no permission", func(f *fakeCompute) {
			f.failNext("PATCH", `/global/firewalls/`, 403, "forbidden", "Required 'compute.firewalls.update' permission for 'the rule'", 1)
		}, nil, "compute.firewalls.update"},
		{"the operation fails", func(f *fakeCompute) { f.failNextOp("patch", "INVALID_FIELD_VALUE", "the patch was refused") }, nil, "the patch was refused"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeCompute(t)
			newRules(f)
			tt.arrange(f)
			c := testClient(t, f)
			err := c.SetEnvFirewallSources(ctx, []string{"203.0.113.9/32"})
			if err == nil {
				t.Fatal("no error")
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("error %v, want %v", err, tt.wantIs)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error %v, want one containing %q", err, tt.wantMsg)
			}
			if rule, rerr := c.EnvFirewallRule(ctx); rerr == nil && !slices.Equal(rule.SourceRanges, []string{"192.0.2.10/32"}) {
				t.Errorf("the rule changed after a failed patch: %+v", rule)
			}
		})
	}
}
