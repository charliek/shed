package remote

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/fake"
)

// stuckRule is the fake with a firewall rule read that can be made to hang,
// as a cloud that has stopped answering does.
type stuckRule struct {
	*fake.Provider
	mu      sync.Mutex
	release chan struct{} // nil: reads answer at once
	entered chan struct{}
}

func (s *stuckRule) EnvFirewallRule(ctx context.Context) (provider.FirewallRule, error) {
	s.mu.Lock()
	release, entered := s.release, s.entered
	s.mu.Unlock()
	if release != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
			return provider.FirewallRule{}, ctx.Err()
		}
	}
	return s.Provider.EnvFirewallRule(ctx)
}

// stick makes rule reads hang until the returned function is called.
func (s *stuckRule) stick() (entered <-chan struct{}, release func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.release, s.entered = make(chan struct{}), make(chan struct{}, 1)
	ch := s.release
	return s.entered, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.release == ch {
			close(ch)
			s.release = nil
		}
	}
}

func stuck(s *stuckRule) envOpt {
	return func(e *env, _ *fake.Options) {
		e.wrap = func(p provider.Provider) provider.Provider {
			s.Provider = p.(*fake.Provider)
			return s
		}
	}
}

// slowCheck starts a periodic-style check that hangs in the cloud, and
// returns once it holds the check.
func slowCheck(t *testing.T, e *env, s *stuckRule, clock *testClock) (release func(), done <-chan struct{}) {
	t.Helper()
	clock.Advance(FirewallMaxAge + time.Second)
	entered, release := s.stick()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		e.b.CheckFirewall(context.Background())
	}()
	select {
	case <-entered:
	case <-time.After(testTimeout):
		t.Fatal("the slow check never reached the cloud")
	}
	t.Cleanup(func() {
		release()
		<-finished
	})
	return release, finished
}

// A create or start that needs a new firewall check while another check is
// in progress waits for that one. A request whose client has gone leaves the
// wait at once; it does not sit behind a check that may take 30 s.
func TestFirewallGateWaitEndsWithTheRequest(t *testing.T) {
	s := &stuckRule{}
	clock := newTestClock()
	e := newEnv(t, stuck(s), withClock(clock))
	slowCheck(t, e, s, clock)

	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() { got <- e.b.firewallGate(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-got:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("firewallGate after its request ended = %v, want context.Canceled", err)
		}
		if errors.Is(err, config.ErrFirewallCheckFailedSentinel) {
			t.Fatalf("a request that ended was told the check failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("firewallGate still waits for the check in progress 2 s after its request ended")
	}
}

// The gate's budget covers the wait for a check in progress, not only the
// check the caller runs itself. A caller whose turn does not come in time is
// refused with the firewall error, and the check in progress is left to
// finish: its result serves the next caller.
func TestFirewallGateBudgetIncludesTheWait(t *testing.T) {
	s := &stuckRule{}
	clock := newTestClock()
	e := newEnv(t, stuck(s), withClock(clock))
	e.b.fw.gateTimeout = 50 * time.Millisecond
	release, done := slowCheck(t, e, s, clock)

	start := time.Now()
	err := e.b.firewallGate(ctxT(t))
	if !errors.Is(err, config.ErrFirewallCheckFailedSentinel) {
		t.Fatalf("firewallGate behind a check that does not finish = %v, want the firewall error", err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("firewallGate waited %s with a budget of %s", waited, e.b.fw.gateTimeout)
	}
	select {
	case <-done:
		t.Fatal("the check in progress was ended by the caller that gave up")
	default:
	}
	// No VM is made on the strength of a check that did not finish.
	if _, err := e.create("refused"); !errors.Is(err, config.ErrFirewallCheckFailedSentinel) {
		t.Fatalf("create behind a check that does not finish = %v, want the firewall error", err)
	}
	if e.instances() != 0 {
		t.Fatal("an instance was made while no check had passed")
	}

	release()
	<-done
	if st := e.b.FirewallStatus(); !st.OK {
		t.Fatalf("the check that was in progress ended %+v, want a pass", st.Failure)
	}
	reads := e.count(fake.OpEnvFirewallRule)
	if err := e.b.firewallGate(ctxT(t)); err != nil {
		t.Fatalf("firewallGate after the check passed: %v", err)
	}
	if n := e.count(fake.OpEnvFirewallRule) - reads; n != 0 {
		t.Fatalf("%d more rule reads, want none: the finished check's result is fresh", n)
	}
}

// A caller that waited for a check in progress uses its result when it is
// fresh, and does not read the cloud again.
func TestFirewallGateUsesTheCheckItWaitedFor(t *testing.T) {
	s := &stuckRule{}
	clock := newTestClock()
	e := newEnv(t, stuck(s), withClock(clock))
	release, done := slowCheck(t, e, s, clock)
	reads := e.count(fake.OpEnvFirewallRule)

	got := make(chan error, 1)
	go func() { got <- e.b.firewallGate(ctxT(t)) }()
	time.Sleep(20 * time.Millisecond)
	release()
	<-done
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("firewallGate after the check it waited for passed: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("firewallGate did not return after the check in progress ended")
	}
	if n := e.count(fake.OpEnvFirewallRule) - reads; n != 1 {
		t.Fatalf("%d rule reads, want 1: the waiter must not run a second check", n)
	}
}
