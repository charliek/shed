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
	"github.com/charliek/shed/internal/remote/state"
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

func (e *env) count(op fake.Op) int { return e.prov.Count(op) }

// TestFirewallCheckClauses has one case for every reason the check can fail,
// and for the things that look wrong and are not.
func TestFirewallCheckClauses(t *testing.T) {
	ingress := func(exclusive bool, sources ...string) func(*fake.Provider) {
		return func(p *fake.Provider) { p.SetIngressState(sources, exclusive) }
	}
	tests := []struct {
		name string
		// sources are static mode's expected ranges; nil means the fake's
		// default source.
		sources []string
		mode    string
		arrange func(p *fake.Provider)
		want    FirewallReason // "" means the check passes
		// wantMsg are words the failure's message must hold.
		wantMsg []string
		// wantExclusive is the status's exclusive.
		wantExclusive bool
	}{
		{name: "as the service applied it", wantExclusive: true},
		{name: "exclusive false passes the check (isolated sheds are gated apart)", arrange: ingress(false, fake.DefaultIngressSource)},

		{name: "another address", arrange: ingress(true, "198.51.100.9/32"),
			want: FirewallSources, wantMsg: []string{"expected sources [192.0.2.1/32]", "found sources [198.51.100.9/32]"}},
		{name: "a second address", arrange: ingress(true, fake.DefaultIngressSource, "198.51.100.9/32"),
			want: FirewallSources, wantMsg: []string{"found sources [192.0.2.1/32, 198.51.100.9/32]"}},
		{name: "everyone", arrange: ingress(true, "0.0.0.0/0"),
			want: FirewallSources, wantMsg: []string{"found sources [0.0.0.0/0]"}},
		{name: "no address", arrange: ingress(true),
			want: FirewallSources, wantMsg: []string{"found sources []"}},
		{name: "one of two expected ranges missing", sources: []string{"192.0.2.1/32", "203.0.113.0/24"},
			want: FirewallSources, wantMsg: []string{"expected sources [192.0.2.1/32, 203.0.113.0/24]"}},
		{name: "two expected ranges in the other order", sources: []string{"203.0.113.0/24", "192.0.2.1/32"},
			arrange: ingress(true, "192.0.2.1/32", "203.0.113.0/24"), wantExclusive: true},

		{name: "the ingress cannot be read", arrange: func(p *fake.Provider) {
			p.FailNext(fake.OpIngress, &provider.Error{Kind: provider.ErrUnavailable, Op: "ingress_get", Code: "UNAVAILABLE", Detail: "HTTP 503"})
		}, want: FirewallUnreadable, wantMsg: []string{"could not read", "HTTP 503"}},

		{name: "mode any does not compare sources", mode: config.RemoteFirewallAny, arrange: ingress(true, "0.0.0.0/0"), wantExclusive: true},
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
			if n := e.count(fake.OpSetIngress); n != 0 {
				t.Errorf("%d writes of the ingress in mode %q", n, tt.mode)
			}
			if tt.want == "" {
				if !status.OK || status.Failure != nil {
					t.Fatalf("the check failed: %v", status.Failure)
				}
				if status.Exclusive != tt.wantExclusive {
					t.Errorf("exclusive %v, want %v", status.Exclusive, tt.wantExclusive)
				}
				return
			}
			if status.OK || status.Failure == nil {
				t.Fatalf("the check passed, want reason %s", tt.want)
			}
			f := status.Failure
			if f.Reason != tt.want {
				t.Errorf("reason %q, want %q (%v)", f.Reason, tt.want, f)
			}
			for _, w := range tt.wantMsg {
				if !strings.Contains(f.Error(), w) {
					t.Errorf("message lacks %q:\n%s", w, f.Error())
				}
			}
		})
	}
}

// An isolated shed needs the service's word that the ingress is exclusive:
// without it, creates and starts of isolated sheds are refused, and open
// ones go on (plan 041 §3.2).
func TestIsolatedNeedsExclusive(t *testing.T) {
	clock := newTestClock()
	e := newEnv(t, withClock(clock))
	ctx := ctxT(t)
	if _, err := e.b.CreateShed(ctx, config.CreateShedRequest{Name: "iso", NoProvision: true, Network: config.NetworkProfileIsolated}); err != nil {
		t.Fatalf("an isolated create with an exclusive ingress: %v", err)
	}
	if got := e.settled("iso").Status; got != config.StatusRunning {
		t.Fatalf("iso settled %q", got)
	}
	if _, err := e.b.StopShed(ctx, "iso"); err != nil {
		t.Fatal(err)
	}
	e.settled("iso")
	e.prov.SetIngressState([]string{fake.DefaultIngressSource}, false)
	clock.Advance(FirewallMaxAge + time.Second)
	_, err := e.b.CreateShed(ctx, config.CreateShedRequest{Name: "iso-2", NoProvision: true, Network: config.NetworkProfileIsolated})
	if !errors.Is(err, config.ErrFirewallCheckFailedSentinel) || !strings.Contains(err.Error(), "exclusive") {
		t.Fatalf("an isolated create without exclusive = %v, want FIREWALL_CHECK_FAILED naming exclusive", err)
	}
	if _, err := e.b.StartShed(ctx, "iso"); !errors.Is(err, config.ErrFirewallCheckFailedSentinel) {
		t.Fatalf("an isolated start without exclusive = %v, want FIREWALL_CHECK_FAILED", err)
	}
	if _, err := e.create("open-one"); err != nil {
		t.Fatalf("an open create without exclusive: %v", err)
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
				Config:   &config.RemoteConfig{Firewall: config.RemoteFirewallConfig{Mode: tt.mode}},
				Provider: fake.New(fake.Options{}), Links: newFakeLinks(), StateDir: t.TempDir(),
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

// brokenSources makes the ingress admit an address the config does not
// name; fixedSources puts it back.
func brokenSources(p *fake.Provider) {
	p.SetIngressState([]string{"198.51.100.9/32"}, true)
}

func fixedSources(p *fake.Provider) {
	p.SetIngressState([]string{fake.DefaultIngressSource}, true)
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
	reads := e.count(fake.OpIngress)
	if _, err := e.create("inside-the-minute"); err != nil {
		t.Fatalf("create on a fresh passing result: %v", err)
	}
	e.settled("inside-the-minute")
	if n := e.count(fake.OpIngress); n != reads {
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
		if !errors.As(err, &f) || f.Reason != FirewallSources {
			t.Fatalf("create error carries %+v, want reason %s", f, FirewallSources)
		}
		for _, w := range []string{"expected sources [192.0.2.1/32]", "found sources [198.51.100.9/32]"} {
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
		if s := e.b.FirewallStatus(); s.OK || s.Failure == nil || s.Failure.Reason != FirewallSources {
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
		for _, w := range []string{"firewall check FAILED", "reason sources", "found sources [198.51.100.9/32]"} {
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
	brokenSources(e.prov)
	e.restart() // fails the test if the backend does not become ready
	ctx := ctxT(t)

	if s := e.b.FirewallStatus(); s.OK || s.Failure == nil || s.Failure.Reason != FirewallSources || s.CheckedAt.IsZero() {
		t.Fatalf("status after start-up = %+v", s)
	}
	if !strings.Contains(strings.Join(e.logLines(), "\n"), "firewall check FAILED (mode static, reason sources)") {
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
	// Eight creates at once: a window long enough on a loaded machine
	// for each first run's sends to end inside it (they are bound to it).
	e := newEnv(t, withClock(clock), limits(16, 32), tune(func(o *Options) { o.FastFailWindow = 5 * time.Second }))
	clock.Advance(FirewallMaxAge + time.Second)
	before := e.count(fake.OpIngress)
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
	if n := e.count(fake.OpIngress) - before; n != 1 {
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

// TestFirewallGateInTheJob: a create or start is let in by the firewall check
// when it is asked for, and may reach the cloud much later: resumed by the
// next server after a restart, or queued behind a stop. The job asks the
// check again before it sends the insert or the start, and sends neither
// while the check fails.
func TestFirewallGateInTheJob(t *testing.T) {
	t.Run("a create resumed after a restart", func(t *testing.T) {
		h := &hookProvider{}
		e := newEnv(t, hooked(h))
		const name = "resumed"
		// The first server dies while the insert is on its way: the record
		// holds the create and names no instance.
		entered := make(chan struct{})
		h.create = func(ctx context.Context, _ provider.Spec) (provider.Pending, provider.Instance, error) {
			close(entered)
			<-ctx.Done()
			return provider.Pending{}, provider.Instance{}, ctx.Err()
		}
		if _, err := e.create(name); err != nil {
			t.Fatalf("create: %v", err)
		}
		<-entered
		// While it is down, the rule is opened to another address.
		brokenSources(e.prov)
		inserts := 0
		h.create = func(ctx context.Context, spec provider.Spec) (provider.Pending, provider.Instance, error) {
			inserts++
			return h.Provider.Create(ctx, spec)
		}
		e.restart()
		e.idle()
		got, _ := e.b.GetShed(ctxT(t), name)
		if inserts != 0 || e.instances() != 0 {
			t.Fatalf("%d inserts sent and %d instances exist while the firewall check fails, want none", inserts, e.instances())
		}
		// A resumed create (its first send may have been delivered) is
		// kept for the next pass, not ended: the clean-up rule.
		if got.Status != config.StatusStarting || got.StatusReason != config.StatusReasonFirewall {
			t.Errorf("came to rest %q (%q), want starting (%s)", got.Status, got.StatusReason, config.StatusReasonFirewall)
		}
		if n := markerCount(t, e.dir); n != 0 {
			t.Errorf("%d delete markers, want none", n)
		}
		// Once the rule is right, the next pass creates it.
		fixedSources(e.prov)
		e.sweep()
		if got := e.settled(name); got.Status != config.StatusRunning || inserts != 1 {
			t.Errorf("after the fix: %q (%q), %d inserts", got.Status, got.StatusReason, inserts)
		}
		if n := e.count(fake.OpDestroy); n != 0 {
			t.Errorf("%d destroy calls, want none: nothing was made", n)
		}
	})

	t.Run("a create resumed after a restart, the check passing", func(t *testing.T) {
		h := &hookProvider{}
		e := newEnv(t, hooked(h))
		const name = "resumed"
		entered := make(chan struct{})
		h.create = func(ctx context.Context, _ provider.Spec) (provider.Pending, provider.Instance, error) {
			close(entered)
			<-ctx.Done()
			return provider.Pending{}, provider.Instance{}, ctx.Err()
		}
		if _, err := e.create(name); err != nil {
			t.Fatalf("create: %v", err)
		}
		<-entered
		h.create = nil
		e.restart()
		if got := e.settled(name); got.Status != config.StatusRunning {
			t.Errorf("came to rest %q (%q), want running", got.Status, got.StatusReason)
		}
	})

	t.Run("a start queued behind a stop", func(t *testing.T) {
		clock := newTestClock()
		e := newEnv(t, manual(), withClock(clock))
		const name = "queued"
		e.put(name, config.StatusStopping)
		// Let in while the check passes, and queued behind the stop.
		if _, err := e.b.StartShed(ctxT(t), name); err != nil {
			t.Fatalf("start while stopping: %v", err)
		}
		// The rule changes before the stop ends.
		brokenSources(e.prov)
		clock.Advance(FirewallMaxAge + time.Second)
		starts := e.count(fake.OpStart)
		e.finish(name)
		got := e.settled(name)
		e.idle()
		if n := e.count(fake.OpStart) - starts; n != 0 {
			t.Fatalf("%d start calls sent while the firewall check fails, want none", n)
		}
		if got.Status != config.StatusStopped || got.StatusReason != config.StatusReasonFirewall {
			t.Errorf("came to rest %q (%q), want stopped (%s)", got.Status, got.StatusReason, config.StatusReasonFirewall)
		}
		if n := e.count(fake.OpStop); n != 1 {
			t.Errorf("%d stop calls, want the one that was asked for", n)
		}
		// Once the rule is right the shed starts.
		fixedSources(e.prov)
		clock.Advance(FirewallMaxAge + time.Second)
		if _, err := e.b.StartShed(ctxT(t), name); err != nil {
			t.Fatalf("start after the fix: %v", err)
		}
		e.finish(name)
		if got := e.settled(name); got.Status != config.StatusRunning {
			t.Errorf("after the fix came to rest %q (%q), want running", got.Status, got.StatusReason)
		}
	})

	t.Run("a start resumed after a restart, the VM already up", func(t *testing.T) {
		e := newEnv(t)
		const name = "up"
		e.running(name)
		id := e.settledID(name)
		// The record of a start that died after the cloud brought the VM up.
		if !e.b.update(name, id, func(r *state.Record) {
			r.Shed.Status = config.StatusStarting
			r.Job = state.JobStart
		}) {
			t.Fatal("no record")
		}
		brokenSources(e.prov)
		e.restart()
		got := e.settled(name)
		e.idle()
		if got.Status != config.StatusRunning {
			t.Errorf("came to rest %q (%q), want running: the check never stops a VM or calls a running one stopped", got.Status, got.StatusReason)
		}
		if n := e.count(fake.OpStop); n != 0 {
			t.Errorf("%d stop calls, want none", n)
		}
	})
}
