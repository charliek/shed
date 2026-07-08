package main

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"testing"
)

// fakeGroups records group lifecycle without real HostClients/SSE.
type fakeGroups struct {
	mu      sync.Mutex
	started []ServerTarget
	stopped []string
}

func (f *fakeGroups) factory(parent context.Context, t ServerTarget, _ SharedDeps) *watcherGroup {
	f.mu.Lock()
	f.started = append(f.started, t)
	f.mu.Unlock()

	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		f.mu.Lock()
		f.stopped = append(f.stopped, t.Name)
		f.mu.Unlock()
		close(done)
	}()
	return &watcherGroup{target: t, cancel: cancel, done: done}
}

func (f *fakeGroups) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started)
}

func (f *fakeGroups) stoppedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.stopped...)
	sort.Strings(out)
	return out
}

func newTestSupervisor(f *fakeGroups) *Supervisor {
	s := NewSupervisor(context.Background(), SharedDeps{Logger: slog.Default()})
	s.newGroup = f.factory
	return s
}

func groupNames(s *Supervisor) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.groups))
	for n := range s.groups {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func TestShouldMint(t *testing.T) {
	minter := &CredentialMinter{} // non-nil; never dialed in this test

	tests := []struct {
		name string
		deps SharedDeps
		tgt  ServerTarget
		want bool
	}{
		{"https + ssh + minter", SharedDeps{Minter: minter}, ServerTarget{URL: "https://h:8443", SSHHost: "h", SSHPort: 2222}, true},
		{"HTTPS uppercase normalized", SharedDeps{Minter: minter}, ServerTarget{URL: "HTTPS://h:8443", SSHHost: "h", SSHPort: 2222}, true},
		{"http is open -> no mint", SharedDeps{Minter: minter}, ServerTarget{URL: "http://h:8080", SSHHost: "h", SSHPort: 2222}, false},
		{"empty url -> no mint", SharedDeps{Minter: minter}, ServerTarget{URL: "", SSHHost: "h", SSHPort: 2222}, false},
		{"nil minter -> no mint", SharedDeps{}, ServerTarget{URL: "https://h:8443", SSHHost: "h", SSHPort: 2222}, false},
		{"no ssh host -> no mint", SharedDeps{Minter: minter}, ServerTarget{URL: "https://h:8443", SSHHost: "", SSHPort: 2222}, false},
		{"no ssh port -> no mint", SharedDeps{Minter: minter}, ServerTarget{URL: "https://h:8443", SSHHost: "h", SSHPort: 0}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldMint(tc.deps, tc.tgt); got != tc.want {
				t.Errorf("shouldMint() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSupervisorReconcileAddRemove(t *testing.T) {
	f := &fakeGroups{}
	s := newTestSupervisor(f)

	s.Reconcile([]ServerTarget{{Name: "mini2", URL: "http://mini2:8080"}, {Name: "mini3", URL: "http://mini3:8080"}})
	if got := groupNames(s); len(got) != 2 {
		t.Fatalf("after add: groups = %v, want 2", got)
	}
	if f.startCount() != 2 {
		t.Fatalf("startCount = %d, want 2", f.startCount())
	}

	// Drop mini3.
	s.Reconcile([]ServerTarget{{Name: "mini2", URL: "http://mini2:8080"}})
	if got := groupNames(s); len(got) != 1 || got[0] != "mini2" {
		t.Fatalf("after remove: groups = %v, want [mini2]", got)
	}
	if got := f.stoppedNames(); len(got) != 1 || got[0] != "mini3" {
		t.Fatalf("stopped = %v, want [mini3]", got)
	}
	// mini2 was not restarted.
	if f.startCount() != 2 {
		t.Errorf("startCount = %d, want 2 (mini2 should not churn)", f.startCount())
	}
}

func TestSupervisorReconcileNoChurn(t *testing.T) {
	f := &fakeGroups{}
	s := newTestSupervisor(f)
	target := []ServerTarget{{Name: "mini2", URL: "http://mini2:8080"}}

	s.Reconcile(target)
	s.Reconcile(target)
	s.Reconcile(target)
	if f.startCount() != 1 {
		t.Errorf("startCount = %d, want 1 (idempotent reconcile)", f.startCount())
	}
}

func TestSupervisorReconcileURLChange(t *testing.T) {
	f := &fakeGroups{}
	s := newTestSupervisor(f)

	s.Reconcile([]ServerTarget{{Name: "mini2", URL: "http://old:8080"}})
	s.Reconcile([]ServerTarget{{Name: "mini2", URL: "http://new:8080"}})

	if f.startCount() != 2 {
		t.Errorf("startCount = %d, want 2 (URL change restarts)", f.startCount())
	}
	if got := f.stoppedNames(); len(got) != 1 || got[0] != "mini2" {
		t.Errorf("stopped = %v, want [mini2]", got)
	}
	s.mu.Lock()
	url := s.groups["mini2"].target.URL
	s.mu.Unlock()
	if url != "http://new:8080" {
		t.Errorf("running URL = %q, want http://new:8080", url)
	}
}

func TestSupervisorReconcileCredentialChange(t *testing.T) {
	// A rotated token or a newly-added TLS pin on the SAME url must restart the
	// watcher so the new credential takes effect (else it stays stale until a
	// process restart — an HTTPS target gaining a pin would stay unpinned).
	f := &fakeGroups{}
	s := newTestSupervisor(f)

	s.Reconcile([]ServerTarget{{Name: "s", URL: "https://h:8443", Token: "t1"}})
	s.Reconcile([]ServerTarget{{Name: "s", URL: "https://h:8443", Token: "t2"}})                              // token rotated
	s.Reconcile([]ServerTarget{{Name: "s", URL: "https://h:8443", Token: "t2", TLSFingerprint: "sha256:aa"}}) // pin added

	if f.startCount() != 3 {
		t.Errorf("startCount = %d, want 3 (token change + pin add each restart)", f.startCount())
	}
	s.mu.Lock()
	got := s.groups["s"].target
	s.mu.Unlock()
	if got.Token != "t2" || got.TLSFingerprint != "sha256:aa" {
		t.Errorf("running target = %+v, want Token=t2 TLSFingerprint=sha256:aa", got)
	}
}

func TestSupervisorReconcileDedup(t *testing.T) {
	f := &fakeGroups{}
	s := newTestSupervisor(f)
	// Two targets with the same name collapse to one group.
	s.Reconcile([]ServerTarget{{Name: "mini2", URL: "http://a:8080"}, {Name: "mini2", URL: "http://b:8080"}})
	if got := groupNames(s); len(got) != 1 {
		t.Errorf("groups = %v, want 1", got)
	}
}

func TestSupervisorShutdown(t *testing.T) {
	f := &fakeGroups{}
	s := newTestSupervisor(f)
	s.Reconcile([]ServerTarget{{Name: "mini2", URL: "http://mini2:8080"}, {Name: "mini3", URL: "http://mini3:8080"}})

	s.Shutdown()
	if got := groupNames(s); len(got) != 0 {
		t.Errorf("after shutdown: groups = %v, want empty", got)
	}
	if got := f.stoppedNames(); len(got) != 2 {
		t.Errorf("stopped = %v, want 2", got)
	}

	// Reconcile after Shutdown is a no-op.
	s.Reconcile([]ServerTarget{{Name: "mini4", URL: "http://mini4:8080"}})
	if got := groupNames(s); len(got) != 0 {
		t.Errorf("reconcile after shutdown started groups: %v", got)
	}
}

func TestSupervisorHealth(t *testing.T) {
	f := &fakeGroups{}
	s := NewSupervisor(context.Background(), SharedDeps{Logger: slog.Default()})
	s.newGroup = f.factory
	defer s.Shutdown()

	s.Reconcile([]ServerTarget{{Name: "mini3", URL: "http://mini3:8080"}, {Name: "mini2", URL: "http://mini2:8080"}})

	h := s.Health()
	if len(h) != 2 {
		t.Fatalf("Health() returned %d servers, want 2", len(h))
	}
	// Sorted by name.
	if h[0].Name != "mini2" || h[1].Name != "mini3" {
		t.Fatalf("Health() not sorted by name: %+v", h)
	}
	if h[0].URL != "http://mini2:8080" {
		t.Fatalf("Health()[0].URL = %q", h[0].URL)
	}
	// fakeGroups builds client-less groups → no namespaces, and no panic.
	if len(h[0].Namespaces) != 0 {
		t.Fatalf("expected no namespaces for a client-less group, got %+v", h[0].Namespaces)
	}
}
