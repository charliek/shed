package remote

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/fake"
)

// testClock is an injected clock a test moves by hand.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// tune adds an edit of the backend's options to a test environment.
func tune(fn func(*Options)) envOpt {
	return func(e *env, _ *fake.Options) {
		prev := e.tune
		e.tune = func(o *Options) {
			if prev != nil {
				prev(o)
			}
			fn(o)
		}
	}
}

func withClock(c *testClock) envOpt {
	return tune(func(o *Options) { o.Now = c.Now })
}

func sshRule(name, tag string, sources ...string) provider.FirewallRule {
	return provider.FirewallRule{
		Name: name, Direction: provider.DirectionIngress, Priority: 1000, SourceRanges: sources,
		TargetTags: []string{tag}, Allowed: []provider.FirewallPorts{{Protocol: "tcp", Ports: []string{"22"}}},
	}
}

func (e *env) count(op fake.Op) int { return e.prov.Count(op) }

// TestFirewallCheckClauses has one case for every reason the check can fail,
// and for the things that look wrong and are not.
func TestFirewallCheckClauses(t *testing.T) {
	const ruleName = "shed-" + testEnv + "-ssh"
	const tag = "shed-" + testEnv
	edit := func(fn func(*provider.FirewallRule)) func(*fake.Provider) {
		return func(p *fake.Provider) { p.EditEnvRule(fn) }
	}
	add := func(r provider.FirewallRule) func(*fake.Provider) {
		return func(p *fake.Provider) { p.AddFirewallRule(r) }
	}
	tests := []struct {
		name string
		// sources are static mode's expected ranges; nil means the fake's
		// default source.
		sources []string
		mode    string
		arrange func(p *fake.Provider)
		want    FirewallReason // "" means the check passes
		// wantRule is the rule the failure names; wantMsg are words its
		// message must hold.
		wantRule string
		wantMsg  []string
	}{
		{name: "as the setup script made it"},

		{name: "rule missing", arrange: func(p *fake.Provider) { p.RemoveFirewallRule(ruleName) },
			want: FirewallRuleMissing, wantRule: ruleName, wantMsg: []string{ruleName, "does not exist", "tcp:22", tag}},
		{name: "rule on another network", arrange: edit(func(r *provider.FirewallRule) { r.Network = "some-other-net" }),
			want: FirewallRuleNetwork, wantRule: ruleName, wantMsg: []string{"expected network " + fake.DefaultNetwork, "found network some-other-net"}},
		{name: "rule is egress", arrange: edit(func(r *provider.FirewallRule) { r.Direction = provider.DirectionEgress }),
			want: FirewallRuleDirection, wantRule: ruleName, wantMsg: []string{"expected direction INGRESS", "found direction EGRESS"}},
		{name: "rule disabled", arrange: edit(func(r *provider.FirewallRule) { r.Disabled = true }),
			want: FirewallRuleDisabled, wantRule: ruleName, wantMsg: []string{"is disabled"}},

		{name: "rule allows a second port", arrange: edit(func(r *provider.FirewallRule) { r.Allowed[0].Ports = []string{"22", "80"} }),
			want: FirewallRuleAllow, wantRule: ruleName, wantMsg: []string{"expected allow tcp:22", "found allow tcp:22,80"}},
		{name: "rule allows a port range", arrange: edit(func(r *provider.FirewallRule) { r.Allowed[0].Ports = []string{"22-23"} }),
			want: FirewallRuleAllow, wantRule: ruleName, wantMsg: []string{"found allow tcp:22-23"}},
		{name: "rule allows every tcp port", arrange: edit(func(r *provider.FirewallRule) { r.Allowed[0].Ports = nil }),
			want: FirewallRuleAllow, wantRule: ruleName, wantMsg: []string{"found allow tcp"}},
		{name: "rule allows udp", arrange: edit(func(r *provider.FirewallRule) { r.Allowed[0].Protocol = "udp" }),
			want: FirewallRuleAllow, wantRule: ruleName, wantMsg: []string{"found allow udp:22"}},
		{name: "rule allows every protocol", arrange: edit(func(r *provider.FirewallRule) {
			r.Allowed = []provider.FirewallPorts{{Protocol: "all"}}
		}), want: FirewallRuleAllow, wantRule: ruleName, wantMsg: []string{"found allow all"}},
		{name: "rule allows a second protocol", arrange: edit(func(r *provider.FirewallRule) {
			r.Allowed = append(r.Allowed, provider.FirewallPorts{Protocol: "icmp"})
		}), want: FirewallRuleAllow, wantRule: ruleName, wantMsg: []string{"found allow tcp:22 icmp"}},
		{name: "rule is a deny rule", arrange: edit(func(r *provider.FirewallRule) { r.Allowed, r.Denied = nil, r.Allowed }),
			want: FirewallRuleAllow, wantRule: ruleName, wantMsg: []string{"found deny tcp:22"}},
		{name: "rule protocol in capitals is the same rule", arrange: edit(func(r *provider.FirewallRule) { r.Allowed[0].Protocol = "TCP" })},

		{name: "rule has no target", arrange: edit(func(r *provider.FirewallRule) { r.TargetTags = nil }),
			want: FirewallRuleTarget, wantRule: ruleName, wantMsg: []string{"expected target tag " + tag, "every instance"}},
		{name: "rule targets another tag", arrange: edit(func(r *provider.FirewallRule) { r.TargetTags = []string{"web"} }),
			want: FirewallRuleTarget, wantRule: ruleName, wantMsg: []string{"found target tags [web]"}},
		{name: "rule targets a second tag", arrange: edit(func(r *provider.FirewallRule) { r.TargetTags = append(r.TargetTags, "web") }),
			want: FirewallRuleTarget, wantRule: ruleName, wantMsg: []string{"found target tags [" + tag + ", web]"}},
		{name: "rule targets a service account", arrange: edit(func(r *provider.FirewallRule) {
			r.TargetServiceAccounts = []string{"sa@example.iam.gserviceaccount.com"}
		}), want: FirewallRuleTarget, wantRule: ruleName, wantMsg: []string{"target service accounts"}},

		{name: "rule admits a source tag", arrange: edit(func(r *provider.FirewallRule) { r.SourceTags = []string{"bastion"} }),
			want: FirewallRuleSourceKind, wantRule: ruleName, wantMsg: []string{"by address only", "bastion"}},
		{name: "rule admits a source service account", arrange: edit(func(r *provider.FirewallRule) {
			r.SourceServiceAccounts = []string{"sa@example.iam.gserviceaccount.com"}
		}), want: FirewallRuleSourceKind, wantRule: ruleName},

		{name: "rule admits another address", arrange: edit(func(r *provider.FirewallRule) { r.SourceRanges = []string{"198.51.100.9/32"} }),
			want: FirewallRuleSources, wantRule: ruleName,
			wantMsg: []string{"expected source ranges [192.0.2.1/32]", "found source ranges [198.51.100.9/32]"}},
		{name: "rule admits a second address", arrange: edit(func(r *provider.FirewallRule) { r.SourceRanges = append(r.SourceRanges, "198.51.100.9/32") }),
			want: FirewallRuleSources, wantRule: ruleName, wantMsg: []string{"found source ranges [192.0.2.1/32, 198.51.100.9/32]"}},
		{name: "rule admits everyone", arrange: edit(func(r *provider.FirewallRule) { r.SourceRanges = []string{"0.0.0.0/0"} }),
			want: FirewallRuleSources, wantRule: ruleName, wantMsg: []string{"found source ranges [0.0.0.0/0]"}},
		{name: "rule admits no address", arrange: edit(func(r *provider.FirewallRule) { r.SourceRanges = nil }),
			want: FirewallRuleSources, wantRule: ruleName, wantMsg: []string{"found source ranges []"}},
		{name: "rule lacks one of two expected ranges", sources: []string{"192.0.2.1/32", "203.0.113.0/24"},
			want: FirewallRuleSources, wantRule: ruleName, wantMsg: []string{"expected source ranges [192.0.2.1/32, 203.0.113.0/24]"}},
		{name: "rule has an ipv6 source", arrange: edit(func(r *provider.FirewallRule) { r.SourceRanges = []string{"2001:db8::/32"} }),
			want: FirewallRuleSources, wantRule: ruleName, wantMsg: []string{"not an IPv4 CIDR"}},
		{name: "two expected ranges in the other order", sources: []string{"203.0.113.0/24", "192.0.2.1/32"},
			arrange: edit(func(r *provider.FirewallRule) { r.SourceRanges = []string{"192.0.2.1/32", "203.0.113.0/24"} })},
		{name: "a bare address is its /32", arrange: edit(func(r *provider.FirewallRule) { r.SourceRanges = []string{"192.0.2.1"} })},
		{name: "a range written with host bits is the same range", sources: []string{"203.0.113.0/24"},
			arrange: edit(func(r *provider.FirewallRule) { r.SourceRanges = []string{"203.0.113.9/24"} })},

		{name: "stray rule to every instance", arrange: add(provider.FirewallRule{
			Name: "allow-internal", Priority: 65534, SourceRanges: []string{"10.128.0.0/9"}, Allowed: []provider.FirewallPorts{{Protocol: "all"}},
		}), want: FirewallStrayRule, wantRule: "allow-internal", wantMsg: []string{"allow-internal", "every instance on the network", "10.128.0.0/9", ruleName}},
		{name: "stray rule to the shed tag", arrange: add(provider.FirewallRule{
			Name: "debug-http", SourceRanges: []string{"0.0.0.0/0"}, TargetTags: []string{"web", tag},
			Allowed: []provider.FirewallPorts{{Protocol: "tcp", Ports: []string{"80", "443"}}},
		}), want: FirewallStrayRule, wantRule: "debug-http", wantMsg: []string{"allow tcp:80,443", "instances tagged " + tag}},
		{name: "stray rule admitting by tag", arrange: add(provider.FirewallRule{
			Name: "shed-to-shed", SourceTags: []string{tag}, TargetTags: []string{tag}, Allowed: []provider.FirewallPorts{{Protocol: "tcp"}},
		}), want: FirewallStrayRule, wantRule: "shed-to-shed", wantMsg: []string{"tags [" + tag + "]"}},
		{name: "a disabled stray is not one", arrange: add(provider.FirewallRule{
			Name: "allow-internal", Disabled: true, SourceRanges: []string{"10.128.0.0/9"}, Allowed: []provider.FirewallPorts{{Protocol: "all"}},
		})},
		{name: "a deny rule is not a stray", arrange: add(provider.FirewallRule{
			Name: "deny-all", SourceRanges: []string{"0.0.0.0/0"}, Denied: []provider.FirewallPorts{{Protocol: "all"}},
		})},
		{name: "an egress rule is not a stray", arrange: add(provider.FirewallRule{
			Name: "egress-all", Direction: provider.DirectionEgress, DestinationRanges: []string{"0.0.0.0/0"}, Allowed: []provider.FirewallPorts{{Protocol: "all"}},
		})},
		{name: "a rule on another network is not a stray", arrange: add(provider.FirewallRule{
			Name: "default-allow-ssh", Network: "default", SourceRanges: []string{"0.0.0.0/0"}, Allowed: []provider.FirewallPorts{{Protocol: "tcp", Ports: []string{"22"}}},
		})},
		{name: "a rule for a service account only is not a stray", arrange: add(provider.FirewallRule{
			Name: "for-a-service", SourceRanges: []string{"0.0.0.0/0"}, TargetServiceAccounts: []string{"sa@example.iam.gserviceaccount.com"},
			Allowed: []provider.FirewallPorts{{Protocol: "tcp", Ports: []string{"443"}}},
		})},
		// Two environments share one network: the other one's rule targets
		// its own tag, even when that environment is open to anyone.
		{name: "another environment in mode any on the same network", arrange: add(sshRule("shed-otherenv-ssh", "shed-otherenv", "0.0.0.0/0"))},

		{name: "the default network", arrange: func(p *fake.Provider) { p.SetDefaultNetwork(true) },
			want: FirewallDefaultNetwork, wantMsg: []string{"default network", "custom-mode network"}},

		{name: "the rule cannot be read", arrange: func(p *fake.Provider) {
			p.FailNext(fake.OpEnvFirewallRule, provider.Errorf(nil, "env_firewall_rule", "HTTP 403: Required 'compute.firewalls.list' permission"))
		}, want: FirewallUnreadable, wantMsg: []string{"could not read", "compute.firewalls.list"}},
		{name: "the network's rules cannot be read", arrange: func(p *fake.Provider) {
			p.FailNext(fake.OpNetworkIngress, errors.New("connection reset"))
		}, want: FirewallUnreadable, wantMsg: []string{"could not read the ingress rules", "connection reset"}},

		{name: "mode any does not compare sources", mode: config.RemoteFirewallAny,
			arrange: edit(func(r *provider.FirewallRule) { r.SourceRanges = []string{"0.0.0.0/0"} })},
		{name: "mode any still checks the rule's shape", mode: config.RemoteFirewallAny,
			arrange: edit(func(r *provider.FirewallRule) { r.Allowed[0].Ports = nil }), want: FirewallRuleAllow, wantRule: ruleName},
		{name: "mode any still refuses a stray rule", mode: config.RemoteFirewallAny, arrange: add(provider.FirewallRule{
			Name: "allow-internal", SourceRanges: []string{"10.128.0.0/9"}, Allowed: []provider.FirewallPorts{{Protocol: "all"}},
		}), want: FirewallStrayRule, wantRule: "allow-internal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, func(e *env, _ *fake.Options) {
				if tt.mode != "" {
					e.cfg.Firewall.Mode = tt.mode
					e.cfg.Firewall.AcknowledgeAny = tt.mode == config.RemoteFirewallAny
				}
			}, tune(func(o *Options) {
				if tt.sources != nil {
					o.FirewallSources = tt.sources
				}
			}))
			if tt.arrange != nil {
				tt.arrange(e.prov)
			}
			status := e.b.CheckFirewall(ctxT(t))
			if got := e.b.FirewallStatus(); got.OK != status.OK || got.Failure != status.Failure || got.CheckedAt.IsZero() {
				t.Errorf("FirewallStatus = %+v, the check returned %+v", got, status)
			}
			if tt.want == "" {
				if !status.OK || status.Failure != nil {
					t.Fatalf("the check failed: %v", status.Failure)
				}
				return
			}
			if status.OK || status.Failure == nil {
				t.Fatalf("the check passed, want reason %s", tt.want)
			}
			f := status.Failure
			if f.Reason != tt.want || f.Rule != tt.wantRule {
				t.Errorf("reason %q rule %q, want %q %q (%v)", f.Reason, f.Rule, tt.want, tt.wantRule, f)
			}
			for _, w := range tt.wantMsg {
				if !strings.Contains(f.Error(), w) {
					t.Errorf("message lacks %q:\n%s", w, f.Error())
				}
			}
			// The check only ever reads in static and any mode.
			if n := e.count(fake.OpSetFirewallSources); n != 0 {
				t.Errorf("%d patches of the rule", n)
			}
		})
	}
}

func TestNewNeedsStaticSources(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		sources []string
		wantErr string
	}{
		{"static with a range", config.RemoteFirewallStatic, []string{"192.0.2.1/32"}, ""},
		{"an unset mode is static", "", []string{"192.0.2.1/32"}, ""},
		{"static with none", config.RemoteFirewallStatic, nil, "lists no source range"},
		{"static with junk", config.RemoteFirewallStatic, []string{"192.0.2.1"}, "IPv4 CIDR"},
		{"any needs none", config.RemoteFirewallAny, nil, ""},
		{"auto without echo urls", config.RemoteFirewallAuto, nil, "echo URLs"},
		{"an unknown mode", "open", nil, "unknown firewall.mode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New(Options{
				Config:   &config.RemoteConfig{Environment: testEnv, Firewall: config.RemoteFirewallConfig{Mode: tt.mode}},
				Provider: fake.New(fake.Options{Env: testEnv}), Links: newFakeLinks(), StateDir: t.TempDir(),
				AuthorizedKeys: []string{testKey}, FirewallSources: tt.sources, SweepInterval: -1,
			})
			if b != nil {
				defer b.Close()
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

// brokenSources makes the environment's rule admit an address the config
// does not name; fixedSources puts it back.
func brokenSources(p *fake.Provider) {
	p.EditEnvRule(func(r *provider.FirewallRule) { r.SourceRanges = []string{"198.51.100.9/32"} })
}

func fixedSources(p *fake.Provider) {
	p.EditEnvRule(func(r *provider.FirewallRule) { r.SourceRanges = []string{fake.DefaultFirewallSource} })
}

// TestFirewallGate: while the check fails, creates and starts are refused and
// nothing else changes; when it passes again they work.
func TestFirewallGate(t *testing.T) {
	clock := newTestClock()
	e := newEnv(t, withClock(clock))
	ctx := ctxT(t)

	for _, name := range []string{"runs", "rests"} {
		if _, err := e.create(name); err != nil {
			t.Fatalf("create %s with a passing check: %v", name, err)
		}
		if got := e.settled(name).Status; got != config.StatusRunning {
			t.Fatalf("%s settled %q", name, got)
		}
	}
	if _, err := e.b.StopShed(ctx, "rests"); err != nil {
		t.Fatal(err)
	}
	if got := e.settled("rests").Status; got != config.StatusStopped {
		t.Fatalf("rests settled %q", got)
	}

	// The rule is changed by hand. A result younger than FirewallMaxAge
	// still stands, and no extra cloud read is made for it.
	brokenSources(e.prov)
	clock.Advance(FirewallMaxAge - time.Second)
	reads := e.count(fake.OpEnvFirewallRule)
	if _, err := e.create("inside-the-minute"); err != nil {
		t.Fatalf("create on a fresh passing result: %v", err)
	}
	e.settled("inside-the-minute")
	if n := e.count(fake.OpEnvFirewallRule); n != reads {
		t.Errorf("a fresh result was checked again (%d reads, was %d)", n, reads)
	}

	// Past the limit, the next create runs the check and is refused.
	clock.Advance(2 * time.Second)
	creates, starts, stops, destroys := e.count(fake.OpCreate), e.count(fake.OpStart), e.count(fake.OpStop), e.count(fake.OpDestroy)

	t.Run("create is refused", func(t *testing.T) {
		_, err := e.create("refused")
		if !errors.Is(err, config.ErrFirewallCheckFailedSentinel) {
			t.Fatalf("create = %v, want the firewall refusal", err)
		}
		var f *FirewallFailure
		if !errors.As(err, &f) || f.Reason != FirewallRuleSources {
			t.Fatalf("create error carries %+v, want reason %s", f, FirewallRuleSources)
		}
		for _, w := range []string{"shed-" + testEnv + "-ssh", "expected source ranges [192.0.2.1/32]", "found source ranges [198.51.100.9/32]"} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("message lacks %q: %v", w, err)
			}
		}
		if _, err := e.b.GetShed(ctx, "refused"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
			t.Errorf("the refused create left a record: %v", err)
		}
		if n := e.count(fake.OpCreate); n != creates {
			t.Errorf("the cloud was asked for a VM (%d creates, was %d)", n, creates)
		}
	})
	t.Run("a taken name is still a conflict", func(t *testing.T) {
		if _, err := e.create("runs"); !errors.Is(err, config.ErrShedAlreadyExistsSentinel) {
			t.Errorf("create of an existing name = %v", err)
		}
	})
	t.Run("start is refused", func(t *testing.T) {
		_, err := e.b.StartShed(ctx, "rests")
		if !errors.Is(err, config.ErrFirewallCheckFailedSentinel) {
			t.Fatalf("start = %v, want the firewall refusal", err)
		}
		if got := e.status("rests"); got != config.StatusStopped {
			t.Errorf("rests is %q after a refused start", got)
		}
		if n := e.count(fake.OpStart); n != starts {
			t.Errorf("the cloud was asked to start a VM")
		}
	})
	t.Run("start of a running shed is answered as before", func(t *testing.T) {
		if _, err := e.b.StartShed(ctx, "runs"); !errors.Is(err, config.ErrShedAlreadyRunningSentinel) {
			t.Errorf("start of a running shed = %v", err)
		}
	})
	t.Run("list and get work", func(t *testing.T) {
		sheds, err := e.b.ListSheds(ctx)
		if err != nil || len(sheds) != 3 {
			t.Errorf("ListSheds = %d sheds, %v", len(sheds), err)
		}
	})
	t.Run("exec reaches the guest", func(t *testing.T) {
		err := e.b.Exec(ctx, "runs", backend.ExecOptions{Cmd: []string{"true"}})
		if errors.Is(err, config.ErrFirewallCheckFailedSentinel) || err == nil || !strings.Contains(err.Error(), "fake agent has no console") {
			t.Errorf("Exec = %v, want the fake guest's own answer", err)
		}
		if _, err := e.b.DialService(ctx, "runs", 80); errors.Is(err, config.ErrFirewallCheckFailedSentinel) {
			t.Errorf("DialService was refused for the firewall: %v", err)
		}
	})
	t.Run("the periodic pass stops and deletes nothing", func(t *testing.T) {
		clock.Advance(FirewallMaxAge + time.Second)
		if err := e.b.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		e.idle()
		if e.count(fake.OpStop) != stops || e.count(fake.OpDestroy) != destroys {
			t.Errorf("the pass stopped or destroyed a VM while the check failed")
		}
		if got := e.status("runs"); got != config.StatusRunning {
			t.Errorf("runs is %q", got)
		}
		if s := e.b.FirewallStatus(); s.OK || s.Failure == nil || s.Failure.Reason != FirewallRuleSources {
			t.Errorf("status after the pass = %+v", s)
		}
	})
	t.Run("stop works", func(t *testing.T) {
		if _, err := e.b.StopShed(ctx, "inside-the-minute"); err != nil {
			t.Fatalf("stop: %v", err)
		}
		if got := e.settled("inside-the-minute").Status; got != config.StatusStopped {
			t.Errorf("settled %q", got)
		}
	})
	t.Run("delete works", func(t *testing.T) {
		if err := e.b.DeleteShed(ctx, "inside-the-minute"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		e.idle()
		if _, err := e.b.GetShed(ctx, "inside-the-minute"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
			t.Errorf("the shed is still there: %v", err)
		}
	})
	t.Run("the failure is in the log", func(t *testing.T) {
		log := strings.Join(e.logLines(), "\n")
		for _, w := range []string{"firewall check FAILED", "reason rule_sources", "found source ranges [198.51.100.9/32]"} {
			if !strings.Contains(log, w) {
				t.Errorf("log lacks %q", w)
			}
		}
	})

	// The operator fixes the rule. The next periodic pass sees it.
	fixedSources(e.prov)
	clock.Advance(FirewallMaxAge + time.Second)
	if err := e.b.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	t.Run("create and start work again", func(t *testing.T) {
		if _, err := e.create("after"); err != nil {
			t.Fatalf("create after the fix: %v", err)
		}
		if got := e.settled("after").Status; got != config.StatusRunning {
			t.Errorf("after settled %q", got)
		}
		if _, err := e.b.StartShed(ctx, "rests"); err != nil {
			t.Fatalf("start after the fix: %v", err)
		}
		if got := e.settled("rests").Status; got != config.StatusRunning {
			t.Errorf("rests settled %q", got)
		}
		if !strings.Contains(strings.Join(e.logLines(), "\n"), "firewall check passed") {
			t.Error("the recovery is not in the log")
		}
	})
}

// TestFirewallGateQueuedStart: a start asked for while the shed is stopping
// would start a VM later, so it is refused too and nothing is queued.
func TestFirewallGateQueuedStart(t *testing.T) {
	clock := newTestClock()
	e := newEnv(t, manual(), withClock(clock))
	e.put("q", config.StatusStopping)
	e.put("up", config.StatusStarting)
	brokenSources(e.prov)
	clock.Advance(FirewallMaxAge + time.Second)
	if _, err := e.b.StartShed(ctxT(t), "q"); !errors.Is(err, config.ErrFirewallCheckFailedSentinel) {
		t.Fatalf("start while stopping = %v, want the firewall refusal", err)
	}
	e.finish("q")
	if got := e.settled("q").Status; got != config.StatusStopped {
		t.Fatalf("q settled %q, want stopped (no start was queued)", got)
	}
	// A start on a shed already on its way up is a no-op and is not gated.
	if _, err := e.b.StartShed(ctxT(t), "up"); err != nil {
		t.Errorf("start of a starting shed = %v", err)
	}
}

// TestFirewallFailingAtStartup: a failing check does not keep the server
// from starting; existing sheds can be listed, stopped and deleted.
func TestFirewallFailingAtStartup(t *testing.T) {
	e := newEnv(t)
	if _, err := e.create("old"); err != nil {
		t.Fatal(err)
	}
	e.settled("old")
	e.prov.RemoveFirewallRule(provider.FirewallRuleName(testEnv))
	e.restart() // fails the test if the backend does not become ready
	ctx := ctxT(t)

	if s := e.b.FirewallStatus(); s.OK || s.Failure == nil || s.Failure.Reason != FirewallRuleMissing || s.CheckedAt.IsZero() {
		t.Fatalf("status after start-up = %+v", s)
	}
	if !strings.Contains(strings.Join(e.logLines(), "\n"), "firewall check FAILED (mode static, reason rule_missing)") {
		t.Error("the start-up failure is not in the log")
	}
	sheds, err := e.b.ListSheds(ctx)
	if err != nil || len(sheds) != 1 {
		t.Fatalf("ListSheds = %v, %v", sheds, err)
	}
	if _, err := e.create("new"); !errors.Is(err, config.ErrFirewallCheckFailedSentinel) {
		t.Errorf("create = %v", err)
	}
	e.settled("old")
	if _, err := e.b.StopShed(ctx, "old"); err != nil {
		t.Errorf("stop: %v", err)
	}
	e.settled("old")
	if err := e.b.DeleteShed(ctx, "old"); err != nil {
		t.Errorf("delete: %v", err)
	}
	e.idle()
}

// TestFirewallGateCheckedOnce: many creates arriving on a stale result share
// one check.
func TestFirewallGateCheckedOnce(t *testing.T) {
	clock := newTestClock()
	e := newEnv(t, withClock(clock), limits(16, 32))
	clock.Advance(FirewallMaxAge + time.Second)
	before := e.count(fake.OpEnvFirewallRule)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.b.CreateShed(context.Background(), config.CreateShedRequest{Name: "c" + string(rune('a'+i)), NoProvision: true})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("create %d: %v", i, err)
		}
	}
	if n := e.count(fake.OpEnvFirewallRule) - before; n != 1 {
		t.Errorf("%d checks for 8 creates, want 1", n)
	}
	e.idle()
}

// TestNetworkProfileWithAcknowledgedRanges: a static config that accepted
// wide ranges cannot promise isolation.
func TestNetworkProfileWithAcknowledgedRanges(t *testing.T) {
	tests := []struct {
		name string
		fw   config.RemoteFirewallConfig
		want bool
	}{
		{"static", config.RemoteFirewallConfig{Mode: config.RemoteFirewallStatic}, true},
		{"static acknowledged", config.RemoteFirewallConfig{Mode: config.RemoteFirewallStatic, AcknowledgeAny: true}, false},
		{"any", config.RemoteFirewallConfig{Mode: config.RemoteFirewallAny, AcknowledgeAny: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, func(e *env, _ *fake.Options) { e.cfg.Firewall = tt.fw })
			if got := e.b.SupportsNetworkProfile(config.NetworkProfileIsolated); got != tt.want {
				t.Errorf("isolated supported = %v, want %v", got, tt.want)
			}
			if !e.b.SupportsNetworkProfile(config.NetworkProfileOpen) {
				t.Error("open is not supported")
			}
		})
	}
}

func TestNormalizeSources(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		want    string
		wantBad string
	}{
		{"sorted and deduplicated", []string{"203.0.113.7/32", "192.0.2.0/24", "203.0.113.7"}, "192.0.2.0/24, 203.0.113.7/32", ""},
		{"host bits masked", []string{"192.0.2.99/24"}, "192.0.2.0/24", ""},
		{"empty", nil, "", ""},
		{"ipv6", []string{"192.0.2.0/24", "2001:db8::/32"}, "", "2001:db8::/32"},
		{"junk", []string{"everyone"}, "", "everyone"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, bad := normalizeSources(tt.in)
			if joinPrefixes(got) != tt.want || bad != tt.wantBad {
				t.Errorf("normalizeSources = %q bad %q, want %q bad %q", joinPrefixes(got), bad, tt.want, tt.wantBad)
			}
		})
	}
}
