package remote

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider/gce"
)

// TestLiveFirewallCheck runs the firewall check against the real cloud, in
// static mode. It only reads: static mode never changes a rule. It runs only
// with SHED_GCE_LIVE=1 and the variables the dev environment file defines.
//
//	set -a; . ~/.config/shed/remote-dev/env; set +a
//	SHED_GCE_LIVE=1 go test ./internal/remote -run TestLiveFirewallCheck -v
func TestLiveFirewallCheck(t *testing.T) {
	if os.Getenv("SHED_GCE_LIVE") != "1" {
		t.Skip("set SHED_GCE_LIVE=1 and the dev environment to run")
	}
	creds, err := gce.LoadCredentials(os.Getenv, "GOOGLE_CREDENTIALS_BASE64", "SHED_GCE_CREDENTIALS_FILE")
	if err != nil {
		t.Fatal(err)
	}
	client, err := gce.New(gce.Config{
		Project: os.Getenv("SHED_GCE_PROJECT"), Zone: os.Getenv("SHED_GCE_ZONE"), Env: os.Getenv("SHED_REMOTE_ENV"),
		Network: os.Getenv("SHED_GCE_NETWORK"), Subnet: os.Getenv("SHED_GCE_SUBNET"), Token: creds, Backoff: []time.Duration{},
	})
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, sources []string) FirewallStatus {
		t.Helper()
		b, err := New(Options{
			Config:   &config.RemoteConfig{Environment: os.Getenv("SHED_REMOTE_ENV"), Firewall: config.RemoteFirewallConfig{Mode: config.RemoteFirewallStatic}},
			Provider: client, Links: newFakeLinks(), StateDir: t.TempDir(), AuthorizedKeys: []string{testKey},
			FirewallSources: sources, SweepInterval: -1, Logf: func(string, ...any) {},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		return b.CheckFirewall(ctx)
	}

	// Every clause but the sources is checked first, so reaching
	// rule_sources shows the real rule and network pass all of them.
	t.Run("a documentation address is not the rule's source", func(t *testing.T) {
		s := check(t, []string{"192.0.2.1/32"})
		if s.OK || s.Failure == nil || s.Failure.Reason != FirewallRuleSources {
			reason := FirewallReason("")
			if s.Failure != nil {
				reason = s.Failure.Reason
			}
			t.Fatalf("OK=%v reason=%q, want reason %s", s.OK, reason, FirewallRuleSources)
		}
	})
	t.Run("the configured sources", func(t *testing.T) {
		value := os.Getenv(config.DefaultRemoteFirewallSourceRangesEnv)
		if strings.TrimSpace(value) == "" {
			t.Skipf("%s is not set", config.DefaultRemoteFirewallSourceRangesEnv)
		}
		sources, err := config.ParseFirewallSources(config.DefaultRemoteFirewallSourceRangesEnv, strings.Split(value, ","), false)
		if err != nil {
			t.Fatal(err)
		}
		if s := check(t, sources); !s.OK {
			// The reason only: the message holds addresses.
			t.Fatalf("the check fails with reason %s", s.Failure.Reason)
		}
	})
}
