package api

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// reaperBackend is clientFakeBackend with a configurable DeleteShed failure
// and a record of the owner each operation was made under.
type reaperBackend struct {
	*clientFakeBackend
	deleteErr error
	owners    []string
}

// requiredOwner reports the owner ctx requires: testClientName, "none" when
// it requires none, "other" otherwise.
func requiredOwner(ctx context.Context, name string) string {
	switch {
	case backend.CheckOwner(ctx, name, "someone-else") == nil:
		return "none"
	case backend.CheckOwner(ctx, name, testClientName) == nil:
		return testClientName
	default:
		return "other"
	}
}

func (b *reaperBackend) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	b.owners = append(b.owners, "stop "+name+" "+requiredOwner(ctx, name))
	return b.clientFakeBackend.StopShed(ctx, name)
}

func (b *reaperBackend) DeleteShed(ctx context.Context, name string) error {
	b.owners = append(b.owners, "delete "+name+" "+requiredOwner(ctx, name))
	if b.deleteErr != nil {
		b.record("delete-failed " + name)
		return b.deleteErr
	}
	return b.clientFakeBackend.DeleteShed(ctx, name)
}

func TestReaperPass(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	maxAge := 30 * time.Hour
	b := &reaperBackend{clientFakeBackend: newClientFakeBackend(
		// Old and owned, running: stopped, then deleted.
		config.Shed{Name: "nocturn-old", Status: config.StatusRunning, Owner: testClientName, CreatedAt: now.Add(-maxAge - time.Minute)},
		// Old and owned, stopped: deleted without a stop.
		config.Shed{Name: "nocturn-old-stopped", Status: config.StatusStopped, Owner: testClientName, CreatedAt: now.Add(-48 * time.Hour)},
		// Young and owned: kept.
		config.Shed{Name: "nocturn-young", Status: config.StatusRunning, Owner: testClientName, CreatedAt: now.Add(-maxAge + time.Minute)},
		// Exactly max_age: kept (only older than max_age goes).
		config.Shed{Name: "nocturn-edge", Status: config.StatusRunning, Owner: testClientName, CreatedAt: now.Add(-maxAge)},
		// Unowned (an admin's), however old: never touched.
		config.Shed{Name: "admin-ancient", Status: config.StatusRunning, CreatedAt: now.Add(-365 * 24 * time.Hour)},
		// Owned by a client that is no longer configured: left alone.
		config.Shed{Name: "gone-old", Status: config.StatusRunning, Owner: "gone", CreatedAt: now.Add(-365 * 24 * time.Hour)},
	)}
	policy := defaultTestPolicy()
	policy.MaxAge = config.Duration(maxAge)
	s, _ := clientTestServer(t, b, policy)
	path := attachTestAudit(t, s)

	s.reapOnce(context.Background(), now)

	wantCalls := []string{"stop nocturn-old", "delete nocturn-old", "delete nocturn-old-stopped"}
	if got := b.callsSnapshot(); !slices.Equal(got, wantCalls) {
		t.Errorf("calls = %v, want %v", got, wantCalls)
	}
	// Each operation requires the owner the listing showed, so the backend
	// refuses a shed recreated by someone else in between.
	wantOwners := []string{"stop nocturn-old nocturn", "delete nocturn-old nocturn", "delete nocturn-old-stopped nocturn"}
	if !slices.Equal(b.owners, wantOwners) {
		t.Errorf("owners = %v, want %v", b.owners, wantOwners)
	}
	left, _ := b.ListSheds(context.Background())
	var names []string
	for _, sh := range left {
		names = append(names, sh.Name)
	}
	if want := []string{"admin-ancient", "gone-old", "nocturn-edge", "nocturn-young"}; !slices.Equal(names, want) {
		t.Errorf("left %v, want %v", names, want)
	}

	_, _, other := auditLines(t, path)
	if len(other) != 2 {
		t.Fatalf("want 2 reap lines, got %v", other)
	}
	r := other[0]
	if r["event"] != "reap" || r["shed"] != "nocturn-old" || r["owner"] != testClientName || r["age_seconds"] != float64((maxAge+time.Minute)/time.Second) {
		t.Errorf("reap line = %v", r)
	}
	if _, ok := r["error"]; ok {
		t.Errorf("a successful reap has no error: %v", r)
	}

	// The next pass, a minute later, finds nothing more until the young shed
	// ages past max_age.
	s.reapOnce(context.Background(), now.Add(time.Minute))
	if got := b.callsSnapshot(); !slices.Equal(got[len(wantCalls):], []string{"stop nocturn-edge", "delete nocturn-edge"}) {
		t.Errorf("after a minute: %v", got)
	}
	s.reapOnce(context.Background(), now.Add(2*time.Minute+time.Second))
	if got := b.callsSnapshot(); got[len(got)-1] != "delete nocturn-young" {
		t.Errorf("after two minutes: %v", got)
	}
	for _, c := range b.callsSnapshot() {
		if c == "stop admin-ancient" || c == "delete admin-ancient" || c == "delete gone-old" || c == "stop gone-old" {
			t.Errorf("touched a shed it must leave alone: %s", c)
		}
	}
}

func TestReaperRecordsDeleteFailure(t *testing.T) {
	now := time.Now()
	b := &reaperBackend{
		clientFakeBackend: newClientFakeBackend(config.Shed{Name: "nocturn-x", Status: config.StatusStopped, Owner: testClientName, CreatedAt: now.Add(-100 * time.Hour)}),
		deleteErr:         errors.New("disk on fire"),
	}
	s, _ := clientTestServer(t, b, defaultTestPolicy())
	path := attachTestAudit(t, s)
	s.reapOnce(context.Background(), now)
	_, _, other := auditLines(t, path)
	if len(other) != 1 || other[0]["error"] != "disk on fire" {
		t.Errorf("reap lines = %v", other)
	}
}

// RunReaper runs a pass at once and then one per tick, with the injected
// clock, and returns when its context ends.
func TestRunReaperUsesClockAndStops(t *testing.T) {
	start := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	b := &reaperBackend{clientFakeBackend: newClientFakeBackend(
		config.Shed{Name: "nocturn-a", Status: config.StatusStopped, Owner: testClientName, CreatedAt: start},
	)}
	policy := defaultTestPolicy()
	policy.MaxAge = config.Duration(time.Hour)
	s, _ := clientTestServer(t, b, policy)

	clock := make(chan time.Time, 4)
	clock <- start                    // first pass: young
	clock <- start.Add(2 * time.Hour) // second pass: expired
	ctx, cancel := context.WithCancel(context.Background())
	passes := make(chan struct{}, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.RunReaper(ctx, 10*time.Millisecond, func() time.Time {
			passes <- struct{}{}
			select {
			case t := <-clock:
				return t
			default:
				return start.Add(2 * time.Hour)
			}
		})
	}()
	deadline := time.After(5 * time.Second)
	for !slices.Contains(b.callsSnapshot(), "delete nocturn-a") {
		select {
		case <-deadline:
			t.Fatalf("the expired shed was never reaped: %v", b.callsSnapshot())
		case <-passes:
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunReaper did not return after its context ended")
	}
	if n := len(b.callsSnapshot()); n != 1 {
		t.Errorf("want one delete, got %v", b.callsSnapshot())
	}
}

// TestReaperLeavesARecreatedShed: the reaper requires the creation time its
// listing saw, so a shed deleted and recreated under the same name by its own
// client (the owner still matches) after the listing, before the stop or
// between the stop and the delete, is a new, young shed and is left alone,
// with no reap recorded.
func TestReaperLeavesARecreatedShed(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	old := config.Shed{Name: "nocturn-x", Status: config.StatusRunning, Owner: testClientName, CreatedAt: now.Add(-100 * time.Hour)}
	young := config.Shed{Name: "nocturn-x", Status: config.StatusRunning, Owner: testClientName, CreatedAt: now.Add(-time.Minute)}

	for _, tc := range []struct {
		name      string
		replaceAt int // the backend call (1: stop, 2: delete) the recreate lands before
		wantCalls []string
	}{
		{"before the stop", 1, nil},
		{"between the stop and the delete", 2, []string{"stop nocturn-x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &reaperBackend{clientFakeBackend: newClientFakeBackend(old)}
			n := 0
			b.onLock = func() {
				n++
				if n == tc.replaceAt {
					b.mu.Lock()
					b.sheds[young.Name] = young
					b.mu.Unlock()
				}
			}
			s, _ := clientTestServer(t, b, defaultTestPolicy())
			path := attachTestAudit(t, s)

			s.reapOnce(context.Background(), now)

			if got := b.callsSnapshot(); !slices.Equal(got, tc.wantCalls) {
				t.Errorf("calls = %v, want %v", got, tc.wantCalls)
			}
			left, _ := b.ListSheds(context.Background())
			if len(left) != 1 || !left[0].CreatedAt.Equal(young.CreatedAt) {
				t.Errorf("the recreated shed was not left alone: %v", left)
			}
			if _, _, other := auditLines(t, path); len(other) != 0 {
				t.Errorf("recorded a reap that did not happen: %v", other)
			}
		})
	}
}

// gatedDeleteBackend blocks DeleteShed until release is closed, signalling
// entered when it starts.
type gatedDeleteBackend struct {
	*clientFakeBackend
	entered, release chan struct{}
}

func (b *gatedDeleteBackend) DeleteShed(ctx context.Context, name string) error {
	close(b.entered)
	<-b.release
	return b.clientFakeBackend.DeleteShed(ctx, name)
}

// TestStartReaperStopWaitsForAReapInProgress: stop (shed-server's shutdown)
// ends the reaper's scheduling but waits for a delete that has begun, so it
// finishes and is recorded before the audit log and the backend close.
func TestStartReaperStopWaitsForAReapInProgress(t *testing.T) {
	b := &gatedDeleteBackend{
		clientFakeBackend: newClientFakeBackend(config.Shed{Name: "nocturn-x", Status: config.StatusStopped, Owner: testClientName, CreatedAt: time.Now().Add(-100 * time.Hour)}),
		entered:           make(chan struct{}),
		release:           make(chan struct{}),
	}
	s, _ := clientTestServer(t, b, defaultTestPolicy())
	path := attachTestAudit(t, s)

	stop := s.StartReaper(time.Hour, time.Now)
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the reaper never began the delete")
	}
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("stop returned while a delete was in progress")
	case <-time.After(100 * time.Millisecond):
	}
	close(b.release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return after the delete finished")
	}
	if got := b.callsSnapshot(); !slices.Equal(got, []string{"delete nocturn-x"}) {
		t.Errorf("calls = %v", got)
	}
	if _, _, other := auditLines(t, path); len(other) != 1 || other[0]["shed"] != "nocturn-x" {
		t.Errorf("reap lines = %v", other)
	}
	stop() // idempotent
}
